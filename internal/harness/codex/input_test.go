package codex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"testing"
	"time"
)

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
