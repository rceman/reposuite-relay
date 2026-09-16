package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// RuntimeKey is the single shared Codex app-server runtime key: one
// app-server process hosts many native threads, one per Relay session.
const RuntimeKey = "codex/default"

// Stable adapter errors (mapped to API codes by the control layer).
var (
	// ErrBusy means the session already has an in-flight turn.
	ErrBusy = errors.New("session busy")
	// ErrNoActiveTurn means cancel was requested with no in-flight turn.
	ErrNoActiveTurn = errors.New("no active turn")
	// ErrNativeSessionLost means exact resume of the recorded native
	// session failed. Relay never substitutes a different thread.
	ErrNativeSessionLost = errors.New("native session unavailable")
	// ErrNoSuchInput means the requested-input ID is unknown or already
	// resolved.
	ErrNoSuchInput = errors.New("unknown requested input")
	// ErrRuntimeUnavailable means the harness runtime could not be started.
	ErrRuntimeUnavailable = errors.New("harness runtime unavailable")
)

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
	Materialize func(m *session.Managed, upd SessionUpdate) error
	// RandRuntimeID generates an ephemeral runtime identity.
	RandRuntimeID func() (string, error)
	// Version is reported in the initialize clientInfo.
	Version string
	// Now is a clock seam.
	Now func() time.Time
}

// SessionUpdate is one durable metadata mutation requested by the
// adapter. Nil fields are left unchanged; BumpGeneration increments the
// session generation in the same atomic write.
type SessionUpdate struct {
	NativeSessionID *string
	Model           *string
	Mode            *string
	State           *string
	BumpGeneration  bool
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
		st = &sessState{m: m, inputs: map[string]*pendingInput{}}
		a.sessions[m.Session.ID] = st
	} else {
		st.m = m
	}
	snap := m.Snapshot()
	st.materialized = snap.NativeSessionID != ""
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

// Metrics returns the last-known metrics for a session (nil when none).
func (a *Adapter) Metrics(sessionID string) *Metrics {
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

// --- event payloads -------------------------------------------------

type messageUserPayload struct {
	Text   string `json:"text"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type messageCompletedPayload struct {
	TurnID string `json:"turnId"`
	ItemID string `json:"itemId,omitempty"`
	Text   string `json:"text,omitempty"`
}

type turnEventPayload struct {
	TurnID string `json:"turnId,omitempty"`
	Error  string `json:"error,omitempty"`
}

type runtimeExitedPayload struct {
	RuntimeID string `json:"runtimeId"`
	Reason    string `json:"reason,omitempty"`
}

type harnessStartedPayload struct {
	RuntimeID      string `json:"runtimeId"`
	NativeThreadID string `json:"nativeThreadId"`
	Model          string `json:"model,omitempty"`
	Resumed        bool   `json:"resumed"`
}

type inputQuestion struct {
	ID       string                    `json:"id"`
	Header   string                    `json:"header"`
	Question string                    `json:"question"`
	Options  []ToolRequestUserInputOpt `json:"options,omitempty"`
	IsOther  bool                      `json:"isOther,omitempty"`
}

type inputRequestedPayload struct {
	InputID    string          `json:"inputId"`
	TurnID     string          `json:"turnId"`
	ItemID     string          `json:"itemId"`
	IsBlocking bool            `json:"isBlocking"`
	Questions  []inputQuestion `json:"questions"`
}

type inputResolvedPayload struct {
	InputID string              `json:"inputId"`
	Answers map[string][]string `json:"answers"`
}

type inputAbortedPayload struct {
	InputID string `json:"inputId"`
	Reason  string `json:"reason"`
}

type nativeSessionPayload struct {
	NativeSessionID string `json:"nativeSessionId"`
	Generation      int    `json:"generation"`
}

type metricsPayload struct {
	Kind string `json:"kind"`
	Metrics
}

// --- runtime lifecycle ----------------------------------------------

// ensureRuntime returns a ready shared app-server, spawning and
// initializing one when none is live. Concurrent callers converge on one
// process (the supervisor owns that convergence).
func (a *Adapter) ensureRuntime(ctx context.Context, cwd string) (*Server, error) {
	a.mu.Lock()
	if srv, ok := a.servers[RuntimeKey]; ok {
		a.mu.Unlock()
		return srv, nil
	}
	a.mu.Unlock()

	cmd, err := a.deps.Command()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	rtID, err := a.deps.RandRuntimeID()
	if err != nil {
		return nil, err
	}
	rt, err := a.deps.Supervisor.Ensure(RuntimeKey, session.HarnessCodex, rtID, true,
		func() (runtime.Harness, error) {
			srv, err := StartServer(cmd, cwd)
			if err != nil {
				return nil, err
			}
			a.mu.Lock()
			a.servers[RuntimeKey] = srv
			a.mu.Unlock()
			go srv.Serve()
			return srv, nil
		})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	srv, ok := a.servers[rt.Key]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: runtime %s has no server", ErrRuntimeUnavailable, rt.Key)
	}
	// Initialization handshake: until it succeeds the runtime is not
	// ready (and is not usable by any session).
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	info, err := srv.Initialize(initCtx, ClientInfo{
		Name:    "reposuite-relay",
		Title:   "RepoSuite Relay",
		Version: a.deps.Version,
	})
	if err != nil {
		_ = a.deps.Supervisor.FailStart(rt.Key)
		a.mu.Lock()
		delete(a.servers, RuntimeKey)
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: initialize: %v%s", ErrRuntimeUnavailable, err, stderrSuffix(srv))
	}
	_ = info
	if err := a.deps.Supervisor.MarkReady(rt.Key); err != nil {
		_ = a.deps.Supervisor.FailStart(rt.Key)
		a.mu.Lock()
		delete(a.servers, RuntimeKey)
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	a.installHandlers(srv)
	return srv, nil
}

const initTimeout = 15 * time.Second

// callTimeout bounds a single control RPC (thread start/resume, turn
// start, interrupt). Turn execution itself is not bounded here — the
// turn completes via notifications.
const callTimeout = 30 * time.Second

func stderrSuffix(srv *Server) string {
	tail := srv.Stderr()
	if tail == "" {
		return ""
	}
	return " (stderr: " + tail + ")"
}

// installHandlers wires notification and server-request handling once
// per app-server process.
func (a *Adapter) installHandlers(srv *Server) {
	conn := srv.Conn()
	conn.SetNotificationHandler(a.handleNotification)
	conn.SetHandler(MethodRequestUserInput, a.handleRequestUserInput)
	conn.SetHandler(MethodCommandApproval, a.handleApproval)
	conn.SetHandler(MethodFileApproval, a.handleApproval)
	conn.SetHandler(MethodPermissions, a.handleApproval)
}

// OnRuntimeGone is the supervisor's runtime-removal notification: it fires
// exactly once per runtime generation, whether the process died on its own
// or Relay stopped it deliberately. The adapter must never keep serving a
// server whose process is gone.
//
//   - an unexpected exit fails in-flight work durably (turn.failed), aborts
//     unresolved requested input, and records runtime.exited;
//   - a deliberate stop (idle sleep, session stop, shutdown) has no
//     in-flight work by construction, so nothing durable is published.
//
// Either way every bound session becomes COLD: the in-memory thread died
// with the process, while a materialized session keeps its exact durable
// native identity for the next resume.
func (a *Adapter) OnRuntimeGone(key string, rt *runtime.Runtime, reason string) {
	a.mu.Lock()
	srv := a.servers[key]
	delete(a.servers, key)
	affected := make([]*sessState, 0, len(a.sessions))
	for _, st := range a.sessions {
		if st.liveKey == key {
			affected = append(affected, st)
		}
	}
	for _, st := range affected {
		st.liveKey = ""
		st.nativeID = "" // the in-memory thread died with the process
		if st.materialized {
			// The durable exact identity survives; a rebind resumes it.
			st.nativeID = st.m.Snapshot().NativeSessionID
		}
	}
	a.mu.Unlock()

	unexpected := reason != runtime.ReasonStopped
	detail := reason
	if unexpected && srv != nil {
		if err := srv.ExitErr(); err != nil && !errors.Is(err, ErrConnClosed) {
			detail = err.Error()
		}
	}
	for _, st := range affected {
		m := st.m
		if !unexpected {
			// Deliberate stop: no in-flight turn can exist (it is a sleep
			// blocker), so only local state is released.
			a.mu.Lock()
			st.current = nil
			a.mu.Unlock()
			continue
		}
		_ = a.publishDurable(m, api.EventRuntimeExited, runtimeExitedPayload{
			RuntimeID: rt.ID, Reason: detail})
		if st.current != nil {
			_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
				TurnID: st.current.turnID, Error: "runtime exited: " + detail})
		}
		a.abortInputs(st, "runtime exited: "+detail)
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
	}
}

// abortInputs publishes input.aborted for every unresolved request and
// drops the mapping (a dead runtime can never answer them).
func (a *Adapter) abortInputs(st *sessState, reason string) {
	a.mu.Lock()
	pending := st.inputs
	st.inputs = map[string]*pendingInput{}
	a.mu.Unlock()
	for id := range pending {
		_ = a.publishDurable(st.m, api.EventInputAborted, inputAbortedPayload{
			InputID: id, Reason: reason})
	}
}

// setSessionState durably records the logical session state. The
// no-change check lives in the daemon's Materialize hook, under MetaMu:
// reading durable metadata here would race with the adapter's own
// notification goroutines.
func (a *Adapter) setSessionState(m *session.Managed, state string) error {
	return a.deps.Materialize(m, SessionUpdate{State: &state})
}

// --- prompt / turn --------------------------------------------------

// PromptResult reports the accepted turn.
type PromptResult struct {
	TurnID         string `json:"turnId"`
	NativeThreadID string `json:"nativeThreadId"`
	RuntimeID      string `json:"runtimeId"`
}

// Prompt accepts a user prompt and submits it as a native turn:
//
//	message.user (durable, accepted by Relay) → runtime ensure/initialize
//	→ thread/start (first prompt) or thread/resume (after a rebind)
//	→ turn/start → in-flight turn state.
//
// A failure after acceptance publishes a durable turn.failed event, so
// the prompt is never erased from history.
func (a *Adapter) Prompt(ctx context.Context, m *session.Managed, text, model, effort string) (PromptResult, error) {
	st := a.state(m.Session.ID)
	if st == nil {
		return PromptResult{}, fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	if st.current != nil {
		turn := st.current.turnID
		a.mu.Unlock()
		return PromptResult{}, fmt.Errorf("%w: turn %s in flight", ErrBusy, turn)
	}
	a.mu.Unlock()

	// The turn is in flight from the moment Relay accepts it: activity is
	// a sleep blocker for the whole submission.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)

	if model == "" {
		model = m.Snapshot().Model
	}
	mode := m.Snapshot().Mode
	if err := a.publishDurable(m, api.EventMessageUser, messageUserPayload{
		Text: text, Model: model, Effort: effort}); err != nil {
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		return PromptResult{}, err
	}

	fail := func(err error) (PromptResult, error) {
		_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{Error: err.Error()})
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
		return PromptResult{}, err
	}

	srv, err := a.ensureRuntime(ctx, m.Session.Cwd)
	if err != nil {
		return fail(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// Reuse the live thread when this exact runtime generation already
	// owns it; otherwise start a new thread or resume the exact recorded
	// native session.
	a.mu.Lock()
	threadID := st.nativeID
	live := st.liveKey == RuntimeKey
	resume := !live && threadID != ""
	a.mu.Unlock()

	approval := ApprovalNever
	var threadModel string
	if !live {
		if threadID == "" {
			res, err := srv.ThreadStart(callCtx, ThreadStartParams{
				Model: model, Cwd: m.Session.Cwd, ApprovalPolicy: approval,
				ServiceTier: mode,
			})
			if err != nil {
				return fail(fmt.Errorf("thread/start: %w%s", err, stderrSuffix(srv)))
			}
			if res.Thread.ID == "" {
				return fail(errors.New("thread/start returned no thread id"))
			}
			threadID = res.Thread.ID
			threadModel = res.Model
		} else {
			res, err := srv.ThreadResume(callCtx, ThreadResumeParams{
				ThreadID: threadID, Model: model, Cwd: m.Session.Cwd,
				ApprovalPolicy: approval, ServiceTier: mode,
			})
			if err != nil {
				return fail(fmt.Errorf("%w: thread/resume %s: %v%s",
					ErrNativeSessionLost, threadID, err, stderrSuffix(srv)))
			}
			if res.Thread.ID != threadID {
				return fail(fmt.Errorf("%w: thread/resume returned %q, want %q",
					ErrNativeSessionLost, res.Thread.ID, threadID))
			}
			threadModel = res.Model
		}
		// Bind only after the thread exists in this generation.
		if err := a.deps.Supervisor.Bind(RuntimeKey, m.Session.ID); err != nil {
			return fail(err)
		}
		a.mu.Lock()
		st.nativeID = threadID
		st.liveKey = RuntimeKey
		a.threads[threadID] = m.Session.ID
		firstTurn := !st.materialized
		st.turnSeq++
		st.current = &activeTurn{nativeThreadID: threadID, firstTurn: firstTurn}
		a.mu.Unlock()
		if err := a.publishDurable(m, api.EventHarnessStarted, harnessStartedPayload{
			RuntimeID: RuntimeKey, NativeThreadID: threadID,
			Model: threadModel, Resumed: resume}); err != nil {
			return fail(err)
		}
	} else {
		a.mu.Lock()
		firstTurn := !st.materialized
		st.turnSeq++
		st.current = &activeTurn{nativeThreadID: threadID, firstTurn: firstTurn}
		a.mu.Unlock()
	}

	turnParams := TurnStartParams{
		ThreadID: threadID,
		Input:    []UserInput{TextInput(text)},
		Model:    model,
		Effort:   effort,
	}
	if mode := m.Snapshot().Mode; mode != "" {
		turnParams.ServiceTierForTurn = mode
	}
	// The session is durably active BEFORE the turn is submitted: the
	// harness may request input (or complete the turn) the instant it
	// accepts, and those handlers write their own logical state. Writing
	// "active" afterwards would overwrite a waiting_input that is already
	// durable.
	if err := a.setSessionState(m, session.StateActive); err != nil {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(err)
	}
	res, err := srv.TurnStart(callCtx, turnParams)
	if err != nil {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(fmt.Errorf("turn/start: %w%s", err, stderrSuffix(srv)))
	}
	if res.Turn.ID == "" {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(errors.New("turn/start returned no turn id"))
	}
	a.mu.Lock()
	if st.current == nil {
		// The runtime died between acceptance and this write: the death
		// handler already failed the turn.
		a.mu.Unlock()
		return PromptResult{}, fmt.Errorf("%w: runtime exited during turn submission",
			ErrRuntimeUnavailable)
	}
	st.current.turnID = res.Turn.ID
	st.current.agentText = ""
	st.current.agentItemID = ""
	turnID := st.current.turnID
	a.mu.Unlock()
	return PromptResult{TurnID: turnID, NativeThreadID: threadID, RuntimeID: RuntimeKey}, nil
}

// Cancel interrupts the in-flight turn of a session. It is serialized
// through the supervisor's mutation blocker so overlapping cancels fail
// with a stable error instead of racing.
func (a *Adapter) Cancel(ctx context.Context, m *session.Managed) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	cur := st.current
	a.mu.Unlock()
	if cur == nil || cur.turnID == "" {
		return ErrNoActiveTurn
	}
	if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
		return err
	}
	defer a.deps.Supervisor.EndMutation(m.Session.ID)

	a.mu.Lock()
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("%w: runtime gone", ErrNoActiveTurn)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := srv.TurnInterrupt(callCtx, TurnInterruptParams{
		ThreadID: cur.nativeThreadID, TurnID: cur.turnID}); err != nil {
		return fmt.Errorf("turn/interrupt: %w", err)
	}
	// The terminal state arrives as turn/completed(status=interrupted) —
	// Relay does not synthesize it locally.
	return nil
}

// --- requested input ------------------------------------------------

// AnswerSelection is one user answer for one requested-input question.
type AnswerSelection struct {
	QuestionID string   `json:"questionId"`
	Answers    []string `json:"answers"`
}

// AnswerInput resolves a native requested-input request by exact Relay
// input ID. The answer is mapped back to the native question IDs and the
// original JSON-RPC request ID.
func (a *Adapter) AnswerInput(ctx context.Context, m *session.Managed, inputID string, sel []AnswerSelection) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	pending, ok := st.inputs[inputID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchInput, inputID)
	}
	a.mu.Unlock()

	known := map[string]bool{}
	for _, q := range pending.questionIDs {
		known[q] = true
	}
	answers := map[string]ToolRequestUserInputAnswer{}
	for _, s := range sel {
		if !known[s.QuestionID] {
			return fmt.Errorf("%w: question %s does not belong to input %s",
				ErrNoSuchInput, s.QuestionID, inputID)
		}
		answers[s.QuestionID] = ToolRequestUserInputAnswer{Answers: s.Answers}
	}
	if len(answers) == 0 {
		return fmt.Errorf("%w: no answers supplied for %s", ErrNoSuchInput, inputID)
	}

	a.mu.Lock()
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("%w: runtime gone", ErrNoSuchInput)
	}
	if err := srv.Conn().Respond(pending.nativeReqID,
		ToolRequestUserInputResponse{Answers: answers}); err != nil {
		return fmt.Errorf("respond to native input request: %w", err)
	}
	a.mu.Lock()
	delete(st.inputs, inputID)
	a.mu.Unlock()

	flat := map[string][]string{}
	for q, ans := range answers {
		flat[q] = ans.Answers
	}
	if err := a.publishDurable(m, api.EventInputResolved, inputResolvedPayload{
		InputID: inputID, Answers: flat}); err != nil {
		return err
	}
	// The turn continues: it is active again, not waiting for input.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	return nil
}

// PendingInputs returns the unresolved input IDs for a session.
func (a *Adapter) PendingInputs(sessionID string) []string {
	st := a.state(sessionID)
	if st == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(st.inputs))
	for id := range st.inputs {
		out = append(out, id)
	}
	return out
}

// --- server→client requests ----------------------------------------

// handleRequestUserInput defers the native response: Relay publishes a
// durable input.requested event, blocks runtime sleep, and answers only
// when a user supplies input.
func (a *Adapter) handleRequestUserInput(_ context.Context, id int64, params json.RawMessage) (any, error) {
	var p ToolRequestUserInputParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("decode requested input: %w", err)
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return nil, fmt.Errorf("requested input for unknown thread %s", p.ThreadID)
	}
	inputID, err := newInputID()
	if err != nil {
		return nil, err
	}
	questions := make([]inputQuestion, 0, len(p.Questions))
	ids := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		questions = append(questions, inputQuestion{
			ID: q.ID, Header: q.Header, Question: q.Question,
			Options: q.Options, IsOther: q.IsOther,
		})
		ids = append(ids, q.ID)
	}
	a.mu.Lock()
	st.inputs[inputID] = &pendingInput{
		relayID: inputID, nativeReqID: id, nativeThread: p.ThreadID,
		turnID: p.TurnID, itemID: p.ItemID, questionIDs: ids,
	}
	a.mu.Unlock()
	// The blocker is registered before the event that announces it: any
	// observer of input.requested also observes waiting_input, and the
	// runtime is unsleepable from this instant.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityWaitingInput)
	_ = a.setSessionState(m, session.StateWaitingInput)
	if err := a.publishDurable(m, api.EventInputRequested, inputRequestedPayload{
		InputID: inputID, TurnID: p.TurnID, ItemID: p.ItemID,
		IsBlocking: p.IsBlocking, Questions: questions}); err != nil {
		return nil, err
	}
	return nil, ErrPendingResponse
}

// handleApproval fails closed. Relay configures approvalPolicy "never",
// so an approval request means the runtime is not honoring the verified
// bypass contract: the request is declined and recorded, never converted
// into human input.
func (a *Adapter) handleApproval(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var probe struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &probe)
	m, st := a.sessionByThread(probe.ThreadID)
	if st != nil {
		_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
			TurnID: probe.TurnID,
			Error:  "native approval request received despite approvalPolicy=never; declined",
		})
	}
	// "decline" is the verified non-approving decision for command and
	// file-change approvals. Permission grants have no decline value —
	// answering with an RPC error is the fail-closed equivalent.
	return map[string]any{"decision": "decline"}, nil
}

// sessionByThread resolves the session owning a native thread.
func (a *Adapter) sessionByThread(threadID string) (*session.Managed, *sessState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sessionID, ok := a.threads[threadID]
	if !ok {
		return nil, nil
	}
	st := a.sessions[sessionID]
	if st == nil {
		return nil, nil
	}
	return st.m, st
}

func newInputID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "in_" + hex.EncodeToString(b[:]), nil
}

// --- notifications --------------------------------------------------

func (a *Adapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case NotifyTurnStarted:
		a.onTurnStarted(params)
	case NotifyAgentMessage:
		a.onAgentMessageDelta(params)
	case NotifyItemCompleted:
		a.onItemCompleted(params)
	case NotifyTurnCompleted:
		a.onTurnCompleted(params)
	case NotifyTokenUsage:
		a.onTokenUsage(params)
	case NotifyRateLimits:
		a.onRateLimits(params)
	case NotifyError:
		a.onError(params)
	case NotifyThreadStarted, NotifyServerReqResolv:
		// Informational: Relay's own events already record thread start
		// (harness.started) and input resolution (input.resolved).
	default:
		// Unknown notifications are ignored deliberately: the app-server
		// adds notifications over time and Relay must not fail on them.
	}
}

func (a *Adapter) onTurnStarted(params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     Turn   `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current == nil {
		st.current = &activeTurn{nativeThreadID: p.ThreadID, turnID: p.Turn.ID}
	} else if st.current.turnID == "" {
		st.current.turnID = p.Turn.ID
	}
	a.mu.Unlock()
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	_ = a.setSessionState(m, session.StateActive)
}

func (a *Adapter) onAgentMessageDelta(params json.RawMessage) {
	var p AgentMessageDeltaNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current != nil {
		st.current.agentText += p.Delta
		if p.ItemID != "" {
			st.current.agentItemID = p.ItemID
		}
	}
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMessageAgentDelta, messageCompletedPayload{
		TurnID: p.TurnID, ItemID: p.ItemID, Text: p.Delta})
}

func (a *Adapter) onItemCompleted(params json.RawMessage) {
	var p ItemCompletedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.Item.Type != "agentMessage" {
		return
	}
	_, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current != nil {
		st.current.agentText = p.Item.Text
		st.current.agentItemID = p.Item.ID
	}
	a.mu.Unlock()
}

func (a *Adapter) onTurnCompleted(params json.RawMessage) {
	var p TurnCompletedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	// The turn stays in flight until its terminal durable record is
	// published: a concurrent prompt must never interleave its own
	// acceptance events with the completion of the previous turn.
	a.mu.Lock()
	cur := st.current
	text, itemID := "", ""
	if cur != nil {
		text, itemID = cur.agentText, cur.agentItemID
	}
	firstTurn := cur != nil && cur.firstTurn
	nativeID := st.nativeID
	a.mu.Unlock()

	// A completed first turn proves the native session is materialized:
	// only now is the exact native identity persisted.
	if p.Turn.Status == "completed" && firstTurn && nativeID != "" && !st.materialized {
		idle := session.StateIdle
		if err := a.deps.Materialize(m, SessionUpdate{
			NativeSessionID: &nativeID, State: &idle, BumpGeneration: true}); err != nil {
			_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
				TurnID: p.Turn.ID, Error: "persist native session: " + err.Error()})
		} else {
			a.mu.Lock()
			st.materialized = true
			a.mu.Unlock()
			_ = a.publishDurable(m, api.EventNativeSession, nativeSessionPayload{
				NativeSessionID: nativeID, Generation: m.Snapshot().Generation})
		}
	}

	switch p.Turn.Status {
	case "completed":
		// Prefer the completed turn's own agent message over accumulated
		// deltas — deltas are a live view, the item is the final text.
		for _, it := range p.Turn.Items {
			if it.Type == "agentMessage" && it.Text != "" {
				text, itemID = it.Text, it.ID
			}
		}
		_ = a.publishDurable(m, api.EventMessageAgentCompleted, messageCompletedPayload{
			TurnID: p.Turn.ID, ItemID: itemID, Text: text})
	case "interrupted":
		_ = a.publishDurable(m, api.EventTurnInterrupted, turnEventPayload{TurnID: p.Turn.ID})
	case "failed":
		msg := "turn failed"
		if p.Turn.Error != nil && p.Turn.Error.Message != "" {
			msg = p.Turn.Error.Message
		}
		_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
			TurnID: p.Turn.ID, Error: msg})
	default:
		// Unknown terminal status: record it rather than inventing a
		// success or failure.
		_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
			TurnID: p.Turn.ID, Error: "turn ended with status " + p.Turn.Status})
	}

	// Terminal record published: the turn is no longer in flight.
	a.mu.Lock()
	st.current = nil
	a.mu.Unlock()
	// Unresolved input cannot outlive its turn.
	a.abortInputs(st, "turn "+p.Turn.Status)
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
	_ = a.setSessionState(m, session.StateIdle)
}

func (a *Adapter) onTokenUsage(params json.RawMessage) {
	var p ThreadTokenUsageUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.metrics == nil {
		st.metrics = &Metrics{}
	}
	st.metrics.ModelContextWindow = p.TokenUsage.ModelContextWindow
	last, total := p.TokenUsage.Last, p.TokenUsage.Total
	st.metrics.Last, st.metrics.Total = &last, &total
	metrics := *st.metrics
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMetricsUpdated, metricsPayload{
		Kind: "tokenUsage", Metrics: metrics})
}

func (a *Adapter) onRateLimits(params json.RawMessage) {
	var p AccountRateLimitsUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	a.mu.Lock()
	var target *sessState
	for _, st := range a.sessions {
		if st.liveKey == RuntimeKey {
			target = st
			break
		}
	}
	if target == nil {
		a.mu.Unlock()
		return
	}
	if target.metrics == nil {
		target.metrics = &Metrics{}
	}
	rl := p.RateLimits
	target.metrics.RateLimits = &rl
	metrics := *target.metrics
	m := target.m
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMetricsUpdated, metricsPayload{
		Kind: "rateLimits", Metrics: metrics})
}

func (a *Adapter) onError(params json.RawMessage) {
	var p struct {
		Message  string `json:"message"`
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.ThreadID == "" {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	_ = a.publishTransient(m, api.EventHarnessError, turnEventPayload{
		TurnID: p.TurnID, Error: p.Message})
}

// ConfigCommand is an accepted configuration change.
type ConfigCommand struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// ApplyConfig validates and accepts a configuration change. The verified
// app-server surface applies model/mode at thread start/resume and, for
// model and effort, per turn (turn/start). There is no verified
// thread-level model mutation while a thread is live, so a change takes
// effect on the next turn or the next resume — Relay records the
// accepted configuration rather than pretending otherwise.
func (a *Adapter) ApplyConfig(_ context.Context, m *session.Managed, cmd ConfigCommand) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	busy := st.current != nil
	a.mu.Unlock()
	if busy {
		return fmt.Errorf("%w: configuration change while a turn is in flight", ErrBusy)
	}
	if snap := m.Snapshot(); snap.Model == cmd.Model && snap.Mode == cmd.Mode {
		return nil
	}
	if err := a.deps.Materialize(m, SessionUpdate{Model: &cmd.Model, Mode: &cmd.Mode}); err != nil {
		return err
	}
	return a.publishDurable(m, api.EventConfigChanged, ConfigCommand{
		Model: cmd.Model, Mode: cmd.Mode})
}

// StopSession releases adapter state for a session that is being deleted:
// an in-flight turn is interrupted (best effort — a deleted session must
// not leave a native turn running), unresolved input is aborted,
// live-thread tracking is dropped, and the shared runtime keeps serving
// other sessions.
func (a *Adapter) StopSession(m *session.Managed) {
	st := a.state(m.Session.ID)
	if st == nil {
		return
	}
	a.mu.Lock()
	cur := st.current
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if cur != nil && cur.turnID != "" && srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		if err := srv.TurnInterrupt(ctx, TurnInterruptParams{
			ThreadID: cur.nativeThreadID, TurnID: cur.turnID}); err != nil {
			diag("interrupt on stop of session %s: %v", m.Session.Key, err)
		}
		cancel()
	}
	a.abortInputs(st, "session stopped")
	a.mu.Lock()
	if st.nativeID != "" {
		delete(a.threads, st.nativeID)
	}
	st.liveKey = ""
	st.current = nil
	a.mu.Unlock()
	a.Forget(m.Session.ID)
}

// Idle reports whether a session has no in-flight native work.
func (a *Adapter) Idle(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return true
	}
	return st.current == nil && len(st.inputs) == 0
}

// DebugState is a diagnostic snapshot (never persisted).
type DebugState struct {
	NativeThreadID string
	Live           bool
	Materialized   bool
	PendingInputs  int
}

// State reports adapter state for tests and diagnostics.
func (a *Adapter) State(sessionID string) DebugState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return DebugState{}
	}
	return DebugState{
		NativeThreadID: st.nativeID,
		Live:           st.liveKey != "",
		Materialized:   st.materialized,
		PendingInputs:  len(st.inputs),
	}
}

// diag prints a bounded diagnostic line to stderr (never a log file).
func diag(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relayd: codex: "+format+"\n", args...)
}
