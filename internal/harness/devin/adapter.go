// Package devin implements the Devin native harness adapter: the `devin acp`
// runtime speaking the Agent Client Protocol over stdio (verified against
// devin 3000.10.31, which links the Rust `agent-client-protocol` 1.0.0 crate).
//
// Relay semantics owned here, including the two verified Devin differences:
//
//  1. Native identity is a human-readable slug, and a Devin session with no
//     completed turn is NOT safely resumable — so the slug is persisted only
//     after the first completed turn proves materialization. A COLD session
//     with no proven identity creates a new native session on its next prompt;
//     it never pretends to resume one.
//  2. `--model` is a PROCESS-level choice, so Devin runtimes are partitioned
//     by model (one runtime key per process model). A model change therefore
//     applies to the next runtime generation, and Relay says so rather than
//     claiming a live mutation it cannot perform.
//
// Devin starts sessions in an approval mode; Relay selects the advertised
// `bypass` mode explicitly when full permissions are configured.
//
// Everything protocol-shaped lives in internal/harness/acp.
package devin

import (
	"strings"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// RuntimeKeyPrefix namespaces Devin runtime generations. The key is suffixed
// with the process-level model, because Devin's model is chosen when the
// process starts: sessions that need different models need different runtimes.
const RuntimeKeyPrefix = "devin/acp/"

// DefaultModelKey is the runtime key for sessions with no model preference:
// the runtime's own configured default model.
const DefaultModelKey = "default"

// RuntimeKeyFor returns the runtime key for a process-level model.
func RuntimeKeyFor(model string) string {
	return RuntimeKeyPrefix + sanitizeModelKey(model)
}

// sanitizeModelKey makes a model id safe for a runtime key (it is an internal
// identity, never a path or a command).
func sanitizeModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return DefaultModelKey
	}
	var b strings.Builder
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return DefaultModelKey
	}
	return out
}

// BypassMode is the Devin mode that grants full permissions. It is selected
// explicitly at materialization when the runtime advertises it.
const BypassMode = "bypass"

// Deps wires the adapter to the daemon facilities it must use. Durable session
// metadata is only ever mutated through Materialize, under MetaMu.
type Deps struct {
	Broker     *events.Broker
	Supervisor *runtime.Supervisor
	// Command resolves the `devin` executable for a process-level model
	// (production: the installed CLI plus `--model`; tests: the fake agent).
	Command func(model string) (acp.Command, error)
	// Materialize performs a durable session metadata mutation.
	Materialize func(m *session.Managed, upd harness.SessionUpdate) error
	// RandRuntimeID generates an ephemeral runtime identity.
	RandRuntimeID func() (string, error)
	// Version is reported in the ACP handshake clientInfo.
	Version string
	// Now is a clock seam.
	Now func() time.Time
}

// Adapter maps the Devin ACP runtime onto canonical Relay sessions. All of its
// state is daemon-memory only.
type Adapter struct {
	deps Deps

	mu sync.Mutex
	// sessions is every tracked Relay session.
	sessions map[string]*sessState
	// byNative maps a native Devin slug to its Relay session: streaming
	// notifications carry the NATIVE id, so routing requires it.
	byNative map[string]string
	// servers is the live runtime generation per runtime key.
	servers map[string]*acp.Server
	// wg counts adapter-owned goroutines: transport readers and spawned
	// turn-completion workers.
	wg sync.WaitGroup
}

// sessState is the adapter's per-session state; nothing here is durable.
type sessState struct {
	m *session.Managed
	// srv/handle is the runtime generation hosting the session; nil means
	// COLD.
	srv    *acp.Server
	handle *acp.Session
	// liveRuntimeKey is the runtime key of the generation that currently
	// owns handle. Ownership is NEVER reconstructed from the desired model:
	// a handle whose generation was partitioned for another process model
	// must not serve this session.
	liveRuntimeKey string
	// nativeID is the exact native slug in force. Until the first completed
	// turn it is PROVISIONAL: in memory only, never durable, because a
	// zero-turn Devin session cannot be resumed.
	nativeID string
	// materialized is true once a completed turn proved the slug resumable.
	materialized bool
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
func (a *Adapter) Name() string { return session.HarnessDevin }

// Track registers a managed session (create or daemon-restart restore). A
// restored session is COLD: its durable slug (when present) was proven
// resumable by a completed turn in an earlier daemon generation.
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

// StopSession releases adapter state for a session being deleted. An in-flight
// turn is interrupted best-effort; a shared runtime keeps serving its other
// sessions.
func (a *Adapter) StopSession(m *session.Managed) {
	a.mu.Lock()
	st := a.sessions[m.Session.ID]
	var handle *acp.Session
	if st != nil {
		handle = st.handle
	}
	a.mu.Unlock()
	if handle != nil {
		_ = handle.Cancel()
		handle.Close()
	}
	a.deps.Supervisor.Unbind(m.Session.ID)
	a.Forget(m.Session.ID)
}

// detach releases a session's native handle and runtime binding WITHOUT
// stopping the shared runtime generation: its other sessions keep using it.
// A proven durable slug survives (it is exactly resumable); an unproven
// in-memory slug does not — a zero-turn Devin session cannot be resumed.
func (a *Adapter) detach(sessionID string) {
	a.mu.Lock()
	st := a.sessions[sessionID]
	if st == nil {
		a.mu.Unlock()
		return
	}
	handle := st.handle
	if st.nativeID != "" {
		delete(a.byNative, st.nativeID)
	}
	st.srv = nil
	st.handle = nil
	st.liveRuntimeKey = ""
	if !st.materialized {
		st.nativeID = ""
	}
	a.mu.Unlock()
	if handle != nil {
		handle.Close()
	}
	a.deps.Supervisor.Unbind(sessionID)
}

// relaySessionFor resolves the Relay session owning a native Devin slug.
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
	LiveRuntimeKey  string
	Materialized    bool
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
		LiveRuntimeKey:  st.liveRuntimeKey,
		Materialized:    st.materialized,
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

// spawnTurn runs completeTurn for turn on a worker goroutine tracked by
// a.wg, so WaitQuiescent drains it.
func (a *Adapter) spawnTurn(m *session.Managed, handle *acp.Session, turn *acp.Turn) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.completeTurn(m, handle, turn)
	}()
}

// WaitQuiescent blocks until no adapter-owned goroutine can still mutate
// session state: every spawned turn-completion worker has finished and
// every transport reader has exited (including a handler in flight for an
// already-dead runtime). Teardown paths call this after stopping runtimes
// so no goroutine can still write durable state.
func (a *Adapter) WaitQuiescent() {
	a.wg.Wait()
}

// publishMetrics publishes a transient metrics update when there is anything to
// report.
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

// compile-time assertion: the adapter satisfies the shared contract.
var _ harness.Adapter = (*Adapter)(nil)
