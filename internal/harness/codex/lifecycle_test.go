package codex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"testing"
)

// TestMaterializationOnFirstCompletedTurn: the exact native identity is
// persisted only after a completed turn proves the thread is real.
func TestMaterializationOnFirstCompletedTurn(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("mat", t.TempDir())

	// Before any prompt the session is COLD with no native identity.
	if got := e.reload(m.Session.ID); got.NativeSessionID != "" || got.Generation != 1 {
		t.Fatalf("cold session = %+v", got)
	}
	res, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeSessionID == "" || res.TurnID == "" {
		t.Fatalf("prompt result = %+v", res)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)

	got := e.reload(m.Session.ID)
	if got.NativeSessionID != res.NativeSessionID {
		t.Fatalf("nativeSessionId = %q, want %q", got.NativeSessionID, res.NativeSessionID)
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
		mm := e.adapter.RawMetrics(m.Session.ID)
		return mm != nil && mm.Total != nil
	})
	metrics := e.adapter.RawMetrics(m.Session.ID)
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
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "hello"}); err != nil {
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
	res1, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	first := e.reload(m.Session.ID)
	if first.NativeSessionID != res1.NativeSessionID {
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
	res2, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.NativeSessionID != res1.NativeSessionID {
		t.Fatalf("resumed thread = %q, want the exact %q", res2.NativeSessionID, res1.NativeSessionID)
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
	res1, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "first"})
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

	_, err = e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "second"})
	if !errors.Is(err, ErrNativeSessionLost) {
		t.Fatalf("err = %v, want ErrNativeSessionLost", err)
	}
	got := e.reload(m.Session.ID)
	if got.NativeSessionID != res1.NativeSessionID {
		t.Fatalf("native id changed to %q", got.NativeSessionID)
	}
	if got.Generation != 2 {
		t.Fatalf("generation = %d, want 2", got.Generation)
	}
	if !e.adapter.Idle(m.Session.ID) {
		t.Fatal("failed prompt must not leave the session busy")
	}
}
