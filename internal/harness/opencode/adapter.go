// Package opencode implements the OpenCode native harness adapter: the
// `opencode acp` runtime speaking the Agent Client Protocol over stdio
// (verified against opencode 1.17.20's own embedded ACP schemas).
//
// Relay semantics owned here: one shared runtime hosting many native
// sessions, exact native identity (`ses_*`) persisted as soon as the runtime
// returns it, exact resume through session/load (including a zero-turn
// session), generation accounting, canonical events, cancellation, model/mode
// configuration through the runtime's advertised config options, and
// permission policy. Everything protocol-shaped is in internal/harness/acp.
//
// Out of scope by design: `opencode serve`, the TUI, Gateway/WSS, and any web
// administration surface. Relay drives only the ACP stdio runtime.
package opencode

import (
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// RuntimeKey is the single shared OpenCode ACP runtime key: one
// `opencode acp` process hosts many native sessions, one per Relay session.
const RuntimeKey = "opencode/acp"

// Deps wires the adapter to the daemon facilities it must use. Durable
// session metadata is only ever mutated through Materialize, under MetaMu.
type Deps struct {
	Broker     *events.Broker
	Supervisor *runtime.Supervisor
	// Command resolves the `opencode` executable (production: the installed
	// CLI; tests: the deterministic fake ACP agent).
	Command func() (acp.Command, error)
	// Materialize performs a durable session metadata mutation.
	Materialize func(m *session.Managed, upd harness.SessionUpdate) error
	// RandRuntimeID generates an ephemeral runtime identity.
	RandRuntimeID func() (string, error)
	// Version is reported in the ACP handshake clientInfo.
	Version string
	// Now is a clock seam.
	Now func() time.Time
}

// Adapter maps the OpenCode ACP runtime onto canonical Relay sessions. All of
// its state is daemon-memory only.
type Adapter struct {
	deps Deps

	mu sync.Mutex
	// sessions is every tracked Relay session.
	sessions map[string]*sessState
	// byNative maps a native ACP session identity to its Relay session:
	// streaming notifications carry the NATIVE id, so routing requires it.
	byNative map[string]string
	// servers is the live runtime generation, if any.
	servers map[string]*acp.Server
}

// sessState is the adapter's per-session state. Nothing here is durable: the
// durable truth is RelaySession (+ store) and it is only written through
// Materialize.
type sessState struct {
	m *session.Managed
	// srv/handle is the runtime generation currently hosting the session's
	// native session; nil means COLD.
	srv    *acp.Server
	handle *acp.Session
	// nativeID is the exact native identity in force. For OpenCode the
	// runtime returns a durable `ses_*` identity from session/new, so it is
	// persisted as soon as it exists.
	nativeID     string
	materialized bool
	// resumed records that the current runtime generation reattached to the
	// exact native session instead of creating one.
	resumed bool
	// turn is the in-flight turn (nil when idle), turnID is its Relay ID.
	turn   *acp.Turn
	turnID string
	// metrics is the last-known accounting.
	metrics *harness.SessionMetrics
}

// NewAdapter builds the adapter with defaults applied.
func NewAdapter(deps Deps) *Adapter {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Adapter{
		deps:     deps,
		sessions: map[string]*sessState{},
		byNative: map[string]string{},
		servers:  map[string]*acp.Server{},
	}
}

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return session.HarnessOpenCode }

// Track registers a managed session (create or daemon-restart restore).
func (a *Adapter) Track(m *session.Managed) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.sessions[m.Session.ID]
	if !ok {
		st = &sessState{m: m}
		a.sessions[m.Session.ID] = st
	} else {
		st.m = m
	}
	snap := m.Snapshot()
	st.nativeID = snap.NativeSessionID
	st.materialized = harness.ValidNativeID(snap.NativeSessionID)
	if st.nativeID != "" {
		a.byNative[st.nativeID] = m.Session.ID
	}
}

// Forget drops a session's adapter state (durable delete).
func (a *Adapter) Forget(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.sessions[sessionID]; st != nil {
		if st.handle != nil {
			st.handle.Close()
		}
		if st.nativeID != "" {
			delete(a.byNative, st.nativeID)
		}
	}
	delete(a.sessions, sessionID)
}

// StopSession releases adapter state for a session being deleted. An
// in-flight turn is interrupted best-effort, and the shared runtime keeps
// serving its other sessions.
func (a *Adapter) StopSession(m *session.Managed) {
	a.mu.Lock()
	st := a.sessions[m.Session.ID]
	var handle *acp.Session
	if st != nil {
		handle = st.handle
	}
	a.mu.Unlock()
	if handle != nil {
		_ = handle.Cancel() // best effort: a deleted session leaves no turn running
		handle.Close()
	}
	a.deps.Supervisor.Unbind(m.Session.ID)
	a.Forget(m.Session.ID)
}

// relaySessionFor resolves the Relay session owning a native ACP session id.
func (a *Adapter) relaySessionFor(nativeID string) (*sessState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.byNative[nativeID]
	if !ok {
		return nil, false
	}
	st, ok := a.sessions[id]
	return st, ok
}

// state returns the session state, or nil when untracked.
func (a *Adapter) state(sessionID string) *sessState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[sessionID]
}

// Metrics implements harness.Adapter (in-memory last-known accounting).
func (a *Adapter) Metrics(sessionID string) *harness.SessionMetrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return nil
	}
	return st.metrics
}

// DebugState is a test/diagnostic projection of adapter memory.
type DebugState struct {
	NativeSessionID string
	Live            bool
	Materialized    bool
	Resumed         bool
	TurnInFlight    bool
	TurnID          string
}

// State returns the adapter's in-memory view of a session.
func (a *Adapter) State(sessionID string) DebugState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return DebugState{}
	}
	return DebugState{
		NativeSessionID: st.nativeID,
		Live:            st.handle != nil,
		Materialized:    st.materialized,
		Resumed:         st.resumed,
		TurnInFlight:    st.turn != nil,
		TurnID:          st.turnID,
	}
}

// publishDurable appends one canonical durable record.
func (a *Adapter) publishDurable(m *session.Managed, typ string, payload any) error {
	raw, err := marshal(payload)
	if err != nil {
		return err
	}
	es, err := a.deps.Broker.Ensure(m)
	if err != nil {
		return err
	}
	_, err = es.PublishDurable(typ, raw)
	return err
}

// publishTransient publishes a live-only event (never persisted).
func (a *Adapter) publishTransient(m *session.Managed, typ string, payload any) error {
	raw, err := marshal(payload)
	if err != nil {
		return err
	}
	es, err := a.deps.Broker.Ensure(m)
	if err != nil {
		return err
	}
	_, err = es.PublishTransient(typ, raw)
	return err
}

// setSessionState persists the logical state.
func (a *Adapter) setSessionState(m *session.Managed, state string) error {
	if m.Snapshot().State == state {
		return nil
	}
	return a.deps.Materialize(m, harness.SessionUpdate{State: &state})
}

// setMetrics replaces the last-known canonical accounting.
func (a *Adapter) setMetrics(sessionID string, metrics *harness.SessionMetrics) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.sessions[sessionID]; st != nil {
		st.metrics = metrics
	}
}

// publishMetrics publishes a transient metrics update when there is anything
// to report.
func (a *Adapter) publishMetrics(m *session.Managed, kind string, metrics *harness.SessionMetrics) {
	if metrics == nil {
		return
	}
	a.setMetrics(m.Session.ID, metrics)
	_ = a.publishTransient(m, api.EventMetricsUpdated, api.MetricsUpdatedPayload{
		Kind:           kind,
		SessionMetrics: *metrics,
	})
}
