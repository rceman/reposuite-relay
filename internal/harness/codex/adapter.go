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
	// wg counts adapter-owned goroutines: each runtime's transport reader.
	wg sync.WaitGroup
	// testBeforeRespond is a test-only seam invoked inside respondNative
	// immediately before the single native response write — deterministic
	// terminal-race barriers without sleeps. Never set in production.
	testBeforeRespond func()
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

// inputPhase is the durable-resolution lifecycle of one requested input.
// Map presence alone was never enough: the commit point splits the
// lifecycle around the native side effect, so the phase — not the map —
// carries who may send, retry, or terminate.
type inputPhase int

const (
	// inputAnnouncing: the entry is installed but its durable
	// input.requested record has not committed yet — the input is not
	// answerable and never abortable (input.aborted must never precede
	// input.requested). A terminal path marks wantAbort; the announcement
	// owner reconciles after its durable operation returns.
	inputAnnouncing inputPhase = iota
	// inputPending: the native request exists and no response was sent.
	inputPending
	// inputResponding: one caller owns the in-flight Respond write;
	// concurrent answers converge on BUSY, terminal paths mark wantAbort.
	inputResponding
	// inputAnswered: Respond succeeded — the native commit point is past.
	// Only the retained sanitized resolution may still commit; Respond is
	// never repeated and input.aborted is never published for it.
	inputAnswered
	// inputCommitting: one caller owns the durable input.resolved append.
	inputCommitting
	// inputAborting: a terminal path owns the durable input.aborted append.
	inputAborting
)

type pendingInput struct {
	relayID      string
	nativeReqID  int64
	nativeThread string
	turnID       string
	itemID       string
	questionIDs  []string
	// secret marks question IDs whose answers must never be persisted —
	// they are forwarded to the native harness and withheld from the
	// durable input.resolved record.
	secret map[string]bool

	phase inputPhase
	// ready is closed exactly once when the entry leaves the announcing
	// phase — either input.requested committed (pending) or the entry
	// was removed. It does NOT mean the input is answerable: terminal
	// intent (wantAbort) may already be set, and AnswerInput must
	// re-check membership, phase, and wantAbort under a.mu after waking.
	ready chan struct{}
	// wantAbort means a terminal path (turn completion, runtime exit,
	// session stop) visited while a side effect owned the input; the
	// side-effect owner reconciles after its write returns. It also
	// marks an entry whose durable terminal append failed — retained so
	// a later retry or daemon-restart reconciliation can still commit.
	wantAbort   bool
	abortReason string
	// resolution is the sanitized durable input.resolved payload, built
	// BEFORE Respond so a commit retry needs no secret plaintext. It is
	// retained only while phase == inputAnswered.
	resolution *inputResolvedPayload
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

// WaitQuiescent blocks until no adapter-owned goroutine can still mutate
// session state: every transport reader has exited (all
// notification/request handlers run on it, including a handler in flight
// for an already-dead runtime). Teardown paths call this after stopping
// runtimes so no handler can still write durable state.
func (a *Adapter) WaitQuiescent() {
	a.wg.Wait()
}

// turnsInFlight reports how many sessions have an in-flight turn. It exists so
// tests can settle a turn's terminal-record writes before their temp root is
// removed.
func (a *Adapter) turnsInFlight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, st := range a.sessions {
		if st.current != nil {
			n++
		}
	}
	return n
}
