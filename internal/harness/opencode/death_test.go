package opencode

import (
	"fmt"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
)

// TestRuntimeDeathTerminalizesExactlyOnce: the generation dies with a turn in
// flight, so the connection-failure path (the pending session/prompt call
// dies) and the OnRuntimeGone path race for the terminal record. Whichever
// wins, the transcript must contain EXACTLY one turn.failed and one
// runtime.exited — never two failures, never a phantom completion — and the
// session must end idle and COLD. Run under -race.
func TestRuntimeDeathTerminalizesExactlyOnce(t *testing.T) {
	for i := 0; i < 100; i++ {
		e, a := newEnv(t, acp.FakeCancel)
		m := newSession(t, e, a, fmt.Sprintf("death-%d", i), t.TempDir())
		if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
			t.Fatalf("iter %d prompt: %v", i, err)
		}
		harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })

		// Force-kill the generation mid-turn: the pending call's failure and
		// OnRuntimeGone now race for terminal ownership.
		if err := e.Supervisor.Stop(RuntimeKey); err != nil {
			t.Fatalf("iter %d stop: %v", i, err)
		}
		e.WaitDurable(m.Session.ID, api.EventTurnFailed)
		e.WaitDurable(m.Session.ID, api.EventRuntimeExited)

		var failed, exited, completed int
		for _, typ := range e.DurableTypes(m.Session.ID) {
			switch typ {
			case api.EventTurnFailed:
				failed++
			case api.EventRuntimeExited:
				exited++
			case api.EventMessageAgentCompleted:
				completed++
			}
		}
		if failed != 1 || exited != 1 || completed != 0 {
			t.Fatalf("iter %d durable counts: failed=%d exited=%d completed=%d",
				i, failed, exited, completed)
		}
		harnessenv.WaitFor(t, "idle and COLD", func() bool {
			return !a.State(m.Session.ID).Live &&
				e.Supervisor.Activity(m.Session.ID) == runtime.ActivityIdle
		})
		if got := e.Reload(m.Session.ID).State; got != "idle" {
			t.Fatalf("iter %d state = %q, want idle", i, got)
		}
	}
}

// TestRuntimeDeathIdleSessionGetsNoTurnFailure: an idle session records only
// the generation's end — no phantom turn failure.
func TestRuntimeDeathIdleSessionGetsNoTurnFailure(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "idle", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })

	if err := e.Supervisor.Stop(RuntimeKey); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventRuntimeExited)
	var failed, exited int
	for _, typ := range e.DurableTypes(m.Session.ID) {
		switch typ {
		case api.EventTurnFailed:
			failed++
		case api.EventRuntimeExited:
			exited++
		}
	}
	if failed != 0 || exited != 1 {
		t.Fatalf("idle death must produce no turn failure: failed=%d exited=%d", failed, exited)
	}
}
