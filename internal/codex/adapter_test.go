package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// env is a minimal daemon-side harness for adapter tests: a real store, a
// real event broker, a real RuntimeSupervisor, and a real spawned fake
// app-server process. No model call is ever made.
type env struct {
	t       *testing.T
	store   *store.Sessions
	broker  *events.Broker
	sup     *runtime.Supervisor
	adapter *Adapter
}

func newEnv(t *testing.T, mode string) *env {
	t.Helper()
	root := t.TempDir()
	ss, err := store.OpenSessions(root, session.NewSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sup := runtime.New()
	e := &env{t: t, store: ss, broker: events.NewBroker(ss), sup: sup}
	e.adapter = NewAdapter(Deps{
		Broker:      e.broker,
		Supervisor:  sup,
		Command:     fakeCommand,
		Materialize: e.materialize,
		Version:     "test",
	})
	sup.OnGone = e.adapter.OnRuntimeGone
	t.Setenv("RELAY_FAKE_APP_SERVER", "1")
	t.Setenv("FAKE_CODEX_MODE", mode)
	t.Cleanup(func() { _ = sup.StopAll() })
	return e
}

// materialize mirrors the daemon's durable metadata mutation.
func (e *env) materialize(m *session.Managed, upd SessionUpdate) error {
	m.MetaMu.Lock()
	defer m.MetaMu.Unlock()
	rs := m.Session
	if upd.NativeSessionID != nil {
		rs.NativeSessionID = *upd.NativeSessionID
	}
	if upd.Model != nil {
		rs.Model = *upd.Model
	}
	if upd.Mode != nil {
		rs.Mode = *upd.Mode
	}
	if upd.State != nil {
		rs.State = *upd.State
	}
	if upd.BumpGeneration {
		rs.Generation++
	}
	rs.UpdatedAt = time.Now().UTC()
	return e.store.Save(rs)
}

// newSession creates one durable Codex session bound to the adapter.
func (e *env) newSession(key, cwd string) *session.Managed {
	e.t.Helper()
	id, err := session.NewSessionID()
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC()
	rs := &session.RelaySession{
		ID: id, Key: key, Harness: session.HarnessCodex, Cwd: cwd,
		State: session.StateIdle, Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.store.Create(rs); err != nil {
		e.t.Fatal(err)
	}
	m := &session.Managed{Session: rs}
	if _, err := e.broker.Ensure(m); err != nil {
		e.t.Fatal(err)
	}
	e.adapter.Track(m)
	return m
}

// durableTypes returns the durable transcript event types in order.
func (e *env) durableTypes(id string) []string {
	e.t.Helper()
	tr, err := e.store.Transcript(id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Type)
	}
	return out
}

// durablePayload returns the payload of the last durable record of a type.
func (e *env) durablePayload(id, typ string) json.RawMessage {
	e.t.Helper()
	tr, err := e.store.Transcript(id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		e.t.Fatal(err)
	}
	var found json.RawMessage
	for _, r := range recs {
		if r.Type == typ {
			found = r.Payload
		}
	}
	return found
}

func (e *env) reload(id string) *session.RelaySession {
	e.t.Helper()
	all, err := e.store.LoadAll()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, rs := range all {
		if rs.ID == id {
			return rs
		}
	}
	e.t.Fatalf("session %s not on disk", id)
	return nil
}

// waitDurable waits until a durable record of the given type exists: the
// canonical event stream is the deterministic ordering point, so state
// asserted after this is stable.
func (e *env) waitDurable(id, typ string) {
	e.t.Helper()
	waitFor(e.t, "durable "+typ, func() bool {
		return e.durablePayload(id, typ) != nil
	})
}

// waitFor polls until cond holds or the deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestMaterializationOnFirstCompletedTurn: the exact native identity is
// persisted only after a completed turn proves the thread is real.
func TestMaterializationOnFirstCompletedTurn(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("mat", t.TempDir())

	// Before any prompt the session is COLD with no native identity.
	if got := e.reload(m.Session.ID); got.NativeSessionID != "" || got.Generation != 1 {
		t.Fatalf("cold session = %+v", got)
	}
	res, err := e.adapter.Prompt(context.Background(), m, "hello", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeThreadID == "" || res.TurnID == "" {
		t.Fatalf("prompt result = %+v", res)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)

	got := e.reload(m.Session.ID)
	if got.NativeSessionID != res.NativeThreadID {
		t.Fatalf("nativeSessionId = %q, want %q", got.NativeSessionID, res.NativeThreadID)
	}
	if got.Generation != 2 {
		t.Fatalf("generation = %d, want 2 (materialization bumps it once)", got.Generation)
	}
	if got.State != session.StateIdle {
		t.Fatalf("state = %s", got.State)
	}
	types := e.durableTypes(m.Session.ID)
	want := []string{api.EventMessageUser, api.EventHarnessStarted,
		api.EventNativeSession, api.EventMessageAgentCompleted}
	if len(types) != len(want) {
		t.Fatalf("durable types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("durable types = %v, want %v", types, want)
		}
	}
	var completed messageCompletedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventMessageAgentCompleted), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Text != "echo: hello" {
		t.Fatalf("completed text = %q", completed.Text)
	}
	waitFor(t, "token usage metrics", func() bool {
		mm := e.adapter.Metrics(m.Session.ID)
		return mm != nil && mm.Total != nil
	})
	metrics := e.adapter.Metrics(m.Session.ID)
	if metrics.Total.TotalTokens != 15 || metrics.ModelContextWindow == nil || *metrics.ModelContextWindow != 200000 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestNoMaterializationOnFailedTurn: a failed first turn must NOT persist
// the native thread — a thread with no completed turn cannot be resumed
// reliably, so Relay keeps it in memory only.
func TestNoMaterializationOnFailedTurn(t *testing.T) {
	e := newEnv(t, "fail-turn")
	m := e.newSession("fail", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "hello", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventTurnFailed)
	got := e.reload(m.Session.ID)
	if got.NativeSessionID != "" {
		t.Fatalf("failed turn persisted nativeSessionId %q", got.NativeSessionID)
	}
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	if p := e.durablePayload(m.Session.ID, api.EventTurnFailed); p == nil {
		t.Fatal("no durable turn.failed record")
	}
}

// TestExactColdResume: after the runtime generation dies, the next prompt
// resumes the EXACT recorded native thread — same ID, new runtime
// generation, generation counter unchanged until materialization again.
func TestExactColdResume(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("resume", t.TempDir())
	res1, err := e.adapter.Prompt(context.Background(), m, "first", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	first := e.reload(m.Session.ID)
	if first.NativeSessionID != res1.NativeThreadID {
		t.Fatalf("native id = %q", first.NativeSessionID)
	}
	rtID1 := ""
	if v, ok := e.sup.View(m.Session.ID); ok {
		rtID1 = v.RuntimeID
	}
	// The runtime dies (daemon restart, crash, or explicit sleep).
	if err := e.sup.StopAll(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.sup.View(m.Session.ID); ok {
		t.Fatal("session must be COLD after the runtime stops")
	}

	e.sup = runtime.New()
	e.sup.OnGone = e.adapter.OnRuntimeGone
	e.adapter.deps.Supervisor = e.sup
	res2, err := e.adapter.Prompt(context.Background(), m, "second", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res2.NativeThreadID != res1.NativeThreadID {
		t.Fatalf("resumed thread = %q, want the exact %q", res2.NativeThreadID, res1.NativeThreadID)
	}
	e.waitDurable(m.Session.ID, api.EventHarnessStarted)
	waitFor(t, "second completion", func() bool { return e.adapter.Idle(m.Session.ID) })
	if v, ok := e.sup.View(m.Session.ID); !ok || v.RuntimeID == rtID1 {
		t.Fatalf("runtime generation not renewed: %+v", v)
	}
	var started harnessStartedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventHarnessStarted), &started); err != nil {
		t.Fatal(err)
	}
	if !started.Resumed {
		t.Fatal("second generation must report a resume, not a fresh thread")
	}
	// Materialization already happened: no second session.native record and
	// no second generation bump.
	if got := e.reload(m.Session.ID); got.Generation != 2 {
		t.Fatalf("generation = %d, want 2", got.Generation)
	}
	n := 0
	for _, typ := range e.durableTypes(m.Session.ID) {
		if typ == api.EventNativeSession {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("session.native records = %d, want 1", n)
	}
}

// TestResumeFailureIsExplicit: a failed exact resume is reported and never
// silently replaced with a different native session.
func TestResumeFailureIsExplicit(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("lost", t.TempDir())
	res1, err := e.adapter.Prompt(context.Background(), m, "first", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)

	// The runtime restarts as a fake that cannot resume.
	os.Setenv("FAKE_CODEX_MODE", "resume-error")
	defer os.Setenv("FAKE_CODEX_MODE", "happy")
	if err := e.sup.StopAll(); err != nil {
		t.Fatal(err)
	}
	e.sup = runtime.New()
	e.sup.OnGone = e.adapter.OnRuntimeGone
	e.adapter.deps.Supervisor = e.sup

	_, err = e.adapter.Prompt(context.Background(), m, "second", "", "")
	if !errors.Is(err, ErrNativeSessionLost) {
		t.Fatalf("err = %v, want ErrNativeSessionLost", err)
	}
	got := e.reload(m.Session.ID)
	if got.NativeSessionID != res1.NativeThreadID {
		t.Fatalf("native id changed to %q", got.NativeSessionID)
	}
	if got.Generation != 2 {
		t.Fatalf("generation = %d, want 2", got.Generation)
	}
	if !e.adapter.Idle(m.Session.ID) {
		t.Fatal("failed prompt must not leave the session busy")
	}
}

// TestRequestedInputBlocksSleepAndResumes: unresolved requested input is a
// hard runtime sleep blocker; answering resumes the turn.
func TestRequestedInputBlocksSleepAndResumes(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("input", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	if got := e.adapter.State(m.Session.ID); got.PendingInputs != 1 {
		t.Fatalf("pending inputs = %d, want 1", got.PendingInputs)
	}
	if a := e.sup.Activity(m.Session.ID); a != runtime.ActivityWaitingInput {
		t.Fatalf("activity = %s", a)
	}
	if got := e.reload(m.Session.ID); got.State != session.StateWaitingInput {
		t.Fatalf("durable state = %s, want waiting_input", got.State)
	}
	// The submission's own state write happens before the turn is
	// submitted, so nothing may flip the durable state back to active.
	time.Sleep(200 * time.Millisecond)
	if got := e.reload(m.Session.ID); got.State != session.StateWaitingInput {
		t.Fatalf("durable state regressed to %s while input is pending", got.State)
	}
	if ok, why := e.sup.CanSleep(RuntimeKey); ok {
		t.Fatalf("runtime must not be sleepable with pending input (why=%s)", why)
	}
	if err := e.sup.StopIfIdle(RuntimeKey); !errors.Is(err, runtime.ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle = %v, want ErrRuntimeBusy", err)
	}
	var req inputRequestedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventInputRequested), &req); err != nil {
		t.Fatal(err)
	}
	if req.InputID == "" || req.TurnID == "" || len(req.Questions) != 1 ||
		req.Questions[0].ID != "q1" || len(req.Questions[0].Options) != 2 {
		t.Fatalf("input payload = %+v", req)
	}

	// A wrong question ID is rejected.
	err := e.adapter.AnswerInput(context.Background(), m, req.InputID,
		[]AnswerSelection{{QuestionID: "nope", Answers: []string{"alpha"}}})
	if !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("err = %v, want ErrNoSuchInput", err)
	}
	if err := e.adapter.AnswerInput(context.Background(), m, req.InputID,
		[]AnswerSelection{{QuestionID: "q1", Answers: []string{"alpha"}}}); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	waitFor(t, "idle activity", func() bool { return e.sup.Activity(m.Session.ID) == runtime.ActivityIdle })
	if a := e.sup.Activity(m.Session.ID); a != runtime.ActivityIdle {
		t.Fatalf("activity = %s after answering", a)
	}
	if p := e.durablePayload(m.Session.ID, api.EventInputResolved); p == nil {
		t.Fatal("no durable input.resolved record")
	}
	if p := e.durablePayload(m.Session.ID, api.EventMessageAgentCompleted); p == nil {
		t.Fatal("turn did not complete after the answer")
	}
	if ok, why := e.sup.CanSleep(RuntimeKey); !ok {
		t.Fatalf("runtime must be sleepable once input is resolved: %s", why)
	}
	// Answering the same input twice is a stable error.
	if err := e.adapter.AnswerInput(context.Background(), m, req.InputID,
		[]AnswerSelection{{QuestionID: "q1", Answers: []string{"alpha"}}}); !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("second answer err = %v, want ErrNoSuchInput", err)
	}
}

// TestCancelInterruptsInFlightTurn: cancel is exact (thread+turn) and the
// terminal state comes from the harness, not from Relay.
func TestCancelInterruptsInFlightTurn(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("cancel", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "hold", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	if err := e.adapter.Cancel(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventTurnInterrupted)
	if p := e.durablePayload(m.Session.ID, api.EventTurnInterrupted); p == nil {
		t.Fatal("no durable turn.interrupted record")
	}
	e.waitDurable(m.Session.ID, api.EventInputAborted)
	waitFor(t, "idle after interrupt", func() bool { return e.adapter.Idle(m.Session.ID) })
	// No turn in flight now.
	if err := e.adapter.Cancel(context.Background(), m); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

// TestRuntimeDeathFailsInFlightWork: process death is authoritative — the
// turn fails durably, input is aborted, and the session projects COLD.
func TestRuntimeDeathFailsInFlightWork(t *testing.T) {
	e := newEnv(t, "die-on-turn")
	m := e.newSession("death", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "die", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventRuntimeExited)
	e.waitDurable(m.Session.ID, api.EventTurnFailed)
	if _, ok := e.sup.View(m.Session.ID); ok {
		t.Fatal("session must project COLD after runtime death")
	}
	if e.sup.Len() != 0 {
		t.Fatalf("runtimes = %d, want 0", e.sup.Len())
	}
	waitFor(t, "idle after runtime death", func() bool { return e.adapter.Idle(m.Session.ID) })
	// The next prompt starts a fresh generation (and a fresh thread, since
	// nothing was materialized).
	os.Setenv("FAKE_CODEX_MODE", "happy")
	defer os.Setenv("FAKE_CODEX_MODE", "die-on-turn")
	res, err := e.adapter.Prompt(context.Background(), m, "again", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventNativeSession)
	if res.NativeThreadID == "" {
		t.Fatal("recovery prompt produced no thread")
	}
	if got := e.reload(m.Session.ID); got.NativeSessionID != res.NativeThreadID {
		t.Fatalf("recovered native id = %q, want %q", got.NativeSessionID, res.NativeThreadID)
	}
}

// TestBusySessionRejectsSecondPrompt: one in-flight turn per session.
func TestBusySessionRejectsSecondPrompt(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("busy", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "one", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	if _, err := e.adapter.Prompt(context.Background(), m, "two", "", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// TestObserverDoesNotWakeColdRuntime: a live event subscriber on a COLD
// session must not start or retain any runtime for it.
func TestObserverDoesNotWakeColdRuntime(t *testing.T) {
	e := newEnv(t, "happy")
	observer := e.newSession("observer", t.TempDir())
	active := e.newSession("active", t.TempDir())

	evs, ok := e.broker.Get(observer.Session.ID)
	if !ok {
		t.Fatal("observer session has no event state")
	}
	sub, err := evs.Subscribe(evs.Cursor())
	if err != nil {
		t.Fatal(err)
	}
	defer evs.Unsubscribe(sub)

	if _, err := e.adapter.Prompt(context.Background(), active, "wake", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(active.Session.ID, api.EventMessageAgentCompleted)

	if _, ok := e.sup.View(observer.Session.ID); ok {
		t.Fatal("observer session gained a runtime binding")
	}
	if e.sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 (only the active session)", e.sup.Len())
	}
	if types := e.durableTypes(observer.Session.ID); len(types) != 0 {
		t.Fatalf("observer session received events: %v", types)
	}
	// The observer can still be prompted later — its own runtime wake.
	if _, err := e.adapter.Prompt(context.Background(), observer, "later", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(observer.Session.ID, api.EventMessageAgentCompleted)
	if _, ok := e.sup.View(observer.Session.ID); !ok {
		t.Fatal("observer session must wake for its own prompt")
	}
}

// TestMultiSessionSharedRuntimeAndDeleteIsolation: several sessions share
// one app-server process; deleting one leaves the others working.
func TestMultiSessionSharedRuntimeAndDeleteIsolation(t *testing.T) {
	e := newEnv(t, "happy")
	sessions := make([]*session.Managed, 0, 3)
	for _, key := range []string{"s1", "s2", "s3"} {
		sessions = append(sessions, e.newSession(key, t.TempDir()))
	}
	for i, m := range sessions {
		if _, err := e.adapter.Prompt(context.Background(), m, "hi", "", ""); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
		e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	}
	if e.sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 shared app-server", e.sup.Len())
	}
	pids := map[int]bool{}
	for _, m := range sessions {
		v, ok := e.sup.View(m.Session.ID)
		if !ok {
			t.Fatalf("session %s has no runtime", m.Session.Key)
		}
		pids[v.PID] = true
	}
	if len(pids) != 1 {
		t.Fatalf("sessions do not share one process: %v", pids)
	}
	// Three distinct native threads, each durably recorded.
	native := map[string]bool{}
	for _, m := range sessions {
		id := e.reload(m.Session.ID).NativeSessionID
		if id == "" || native[id] {
			t.Fatalf("session %s native id %q (duplicate or empty)", m.Session.Key, id)
		}
		native[id] = true
	}
	// Delete one session: the shared runtime survives and the others keep
	// their exact identity.
	victim := sessions[1]
	e.adapter.StopSession(victim)
	if err := e.sup.StopSession(victim.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(victim.Session.ID); err != nil {
		t.Fatal(err)
	}
	e.broker.Remove(victim.Session.ID)
	if e.sup.Len() != 1 {
		t.Fatal("shared runtime died with one session")
	}
	for _, m := range []*session.Managed{sessions[0], sessions[2]} {
		if _, ok := e.sup.View(m.Session.ID); !ok {
			t.Fatalf("session %s lost its runtime binding", m.Session.Key)
		}
		before := e.reload(m.Session.ID).NativeSessionID
		if _, err := e.adapter.Prompt(context.Background(), m, "more", "", ""); err != nil {
			t.Fatalf("session %s broken after sibling delete: %v", m.Session.Key, err)
		}
		waitFor(t, "idle", func() bool { return e.adapter.Idle(m.Session.ID) })
		if after := e.reload(m.Session.ID).NativeSessionID; after != before {
			t.Fatalf("session %s native identity drifted %q -> %q", m.Session.Key, before, after)
		}
	}
}

// TestConfigChangeIsAcceptedAndDurable: model/mode changes are recorded
// and applied on the next turn; a change during a turn is refused.
func TestConfigChangeIsAcceptedAndDurable(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("cfg", t.TempDir())
	if err := e.adapter.ApplyConfig(context.Background(), m, ConfigCommand{Model: "m1", Mode: "fast"}); err != nil {
		t.Fatal(err)
	}
	got := e.reload(m.Session.ID)
	if got.Model != "m1" || got.Mode != "fast" {
		t.Fatalf("config = %q/%q", got.Model, got.Mode)
	}
	if p := e.durablePayload(m.Session.ID, api.EventConfigChanged); p == nil {
		t.Fatal("no durable session.config record")
	}
	// The accepted model is what the native thread start receives.
	if _, err := e.adapter.Prompt(context.Background(), m, "hello", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventHarnessStarted)
	var started harnessStartedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventHarnessStarted), &started); err != nil {
		t.Fatal(err)
	}
	if started.Model != "m1" {
		t.Fatalf("thread started with model %q, want m1", started.Model)
	}
}
