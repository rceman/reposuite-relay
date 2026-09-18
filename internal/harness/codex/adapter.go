package codex

import (
	"fmt"
	"os"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// RuntimeKey is the single shared Codex app-server runtime key: one
// app-server process hosts many native threads, one per Relay session.
const RuntimeKey = "codex/default"

// Name implements harness.Adapter.
func (a *Adapter) Name() string { return session.HarnessCodex }

// Deps wires the adapter to the daemon facilities it must use. The
// adapter never writes durable session state directly: it calls the
// daemon-owned hooks so metadata mutation stays under MetaMu.
type Deps struct {
	Broker     *events.Broker
	Supervisor *runtime.Supervisor
	// Command resolves the app-server executable (production: the
	// installed Codex CLI; tests: a deterministic fake).
	Command func() (Command, error)
	// Materialize performs a durable session metadata mutation under the
	// session's MetaMu. The adapter never writes session state itself.
	Materialize func(m *session.Managed, upd harness.SessionUpdate) error
	// RandRuntimeID generates an ephemeral runtime identity.
	RandRuntimeID func() (string, error)
	// Version is reported in the initialize clientInfo.
	Version string
	// Now is a clock seam.
	Now func() time.Time
}

// Adapter maps Codex app-server protocol traffic to canonical Relay
// session events. All of its state is daemon-memory only.
type Adapter struct {
	deps Deps

	mu       sync.Mutex
	sessions map[string]*sessState // by RelaySession ID
	threads  map[string]string     // native thread ID -> RelaySession ID
	servers  map[string]*Server    // by runtime key
}

type sessState struct {
	m *session.Managed
	// nativeID is the exact native thread ID. It becomes durable only
	// after materialization is proven; before that it is memory-only and
	// deliberately not persisted (a thread with no completed turn cannot
	// be reliably resumed).
	nativeID string
	// liveKey is the runtime key whose process currently holds the live
	// thread ("" when cold) — resume is only used after a rebind.
	liveKey string
	// materialized mirrors the durable NativeSessionID presence.
	materialized bool

	current *activeTurn
	inputs  map[string]*pendingInput // by Relay input ID
	metrics *Metrics

	// turnSeq numbers turns for deterministic event correlation.
	turnSeq int
}

type activeTurn struct {
	nativeThreadID string
	turnID         string
	firstTurn      bool // this turn proves materialization
	agentText      string
	agentItemID    string
}

type pendingInput struct {
	relayID      string
	nativeReqID  int64
	nativeThread string
	turnID       string
	itemID       string
	questionIDs  []string
}

// Metrics is the last-known in-memory harness accounting for a session.
type Metrics struct {
	ModelContextWindow *int64               `json:"modelContextWindow,omitempty"`
	Last               *TokenUsageBreakdown `json:"last,omitempty"`
	Total              *TokenUsageBreakdown `json:"total,omitempty"`
	RateLimits         *RateLimitSnapshot   `json:"rateLimits,omitempty"`
}

// NewAdapter builds an adapter. Every dependency is required; there are
// no silent defaults for daemon facilities.
func NewAdapter(deps Deps) *Adapter {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.RandRuntimeID == nil {
		deps.RandRuntimeID = newRuntimeID
	}
	return &Adapter{
		deps:     deps,
		sessions: map[string]*sessState{},
		threads:  map[string]string{},
		servers:  map[string]*Server{},
	}
}

func newRuntimeID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rt_" + hex.EncodeToString(b[:]), nil
}

// Track registers a managed session with the adapter (create or restore).
func (a *Adapter) Track(m *session.Managed) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[m.Session.ID]
	if st == nil {
		st = &sessState{
			m:      m,
			inputs: map[string]*pendingInput{},
		}
		a.sessions[m.Session.ID] = st
	} else {
		st.m = m
	}
	snap := m.Snapshot()
	st.materialized = harness.ValidNativeID(snap.NativeSessionID)
	if st.nativeID == "" {
		st.nativeID = snap.NativeSessionID // exact identity from disk
	}
}

// Forget drops a session's adapter state (durable delete).
func (a *Adapter) Forget(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forgetLocked(sessionID)
}

func (a *Adapter) forgetLocked(sessionID string) {
	if st := a.sessions[sessionID]; st != nil {
		if st.nativeID != "" {
			delete(a.threads, st.nativeID)
		}
	}
	delete(a.sessions, sessionID)
}

// Metrics returns the canonical projection of the last-known metrics for
// a session (nil when none). It implements harness.Adapter; the rich
// app-server-specific accounting stays available through RawMetrics.
func (a *Adapter) Metrics(sessionID string) *harness.SessionMetrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil || st.metrics == nil {
		return nil
	}
	if proj := canonicalMetrics(st.metrics); proj != nil {
		return proj
	}
	return nil
}

// canonicalMetrics projects the vendor-rich app-server accounting onto the
// canonical optional shape. Absent measurements stay absent.
func canonicalMetrics(m *Metrics) *harness.SessionMetrics {
	if m == nil {
		return nil
	}
	out := &harness.SessionMetrics{}
	if m.ModelContextWindow != nil {
		out.ContextLimit = harness.Int64(*m.ModelContextWindow)
	}
	if m.Last != nil {
		out.InputTokens = harness.Int64(m.Last.InputTokens)
		out.OutputTokens = harness.Int64(m.Last.OutputTokens)
		out.ReasoningTokens = harness.Int64(m.Last.ReasoningOutputTokens)
		out.CacheTokens = harness.Int64(m.Last.CachedInputTokens)
	}
	if m.RateLimits != nil && m.RateLimits.Primary != nil && m.RateLimits.Primary.UsedPercent != nil {
		used := *m.RateLimits.Primary.UsedPercent
		out.QuotaUsed = &used
	}
	return out
}

// RawMetrics returns the last-known app-server accounting for a session
// (nil when none).
func (a *Adapter) RawMetrics(sessionID string) *Metrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return nil
	}
	return st.metrics
}

// state returns the session state, or nil when untracked.
func (a *Adapter) state(sessionID string) *sessState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[sessionID]
}

// eventSession resolves the canonical event session for a Relay session.
func (a *Adapter) eventSession(m *session.Managed) (*events.Session, error) {
	return a.deps.Broker.Ensure(m)
}

func (a *Adapter) publishDurable(m *session.Managed, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	evs, err := a.eventSession(m)
	if err != nil {
		return err
	}
	_, err = evs.PublishDurable(typ, raw)
	return err
}

func (a *Adapter) publishTransient(m *session.Managed, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	evs, err := a.eventSession(m)
	if err != nil {
		return err
	}
	_, err = evs.PublishTransient(typ, raw)
	return err
}

// diag prints a bounded diagnostic line to stderr (never a log file).
func diag(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relayd: codex: "+format+"\n", args...)
}
