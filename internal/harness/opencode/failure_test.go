package opencode

import (
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestRuntimeDeathFailsTurnDurablyAndGoesCold: a crash fails the in-flight
// turn and leaves the session COLD with its exact identity intact.
func TestRuntimeDeathFailsTurnDurablyAndGoesCold(t *testing.T) {
	e, a := newEnv(t, acp.FakeDieOnPrompt)
	m := newSession(t, e, a, "die", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("die")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnFailed)
	e.WaitDurable(m.Session.ID, api.EventRuntimeExited)
	harnessenv.WaitFor(t, "session COLD", func() bool { return !a.State(m.Session.ID).Live })
	if got := e.Reload(m.Session.ID).State; got != session.StateIdle {
		t.Fatalf("state = %q, want idle", got)
	}
	if e.Supervisor.Activity(m.Session.ID) != "idle" {
		t.Fatalf("activity = %q", e.Supervisor.Activity(m.Session.ID))
	}
}

// TestSecondPromptReusesLiveGeneration: a warm generation is reused without a
// second native bind, so the generation does not advance.
func TestSecondPromptReusesLiveGeneration(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "reuse", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "first turn settled", func() bool {
		return !a.State(m.Session.ID).TurnInFlight
	})
	if _, err := a.Prompt(bg(), m, text("two")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "second completion", func() bool {
		return len(e.DurablePayloads(m.Session.ID, api.EventMessageAgentCompleted)) == 2
	})
	if got := e.Reload(m.Session.ID).Generation; got != 1 {
		t.Fatalf("generation = %d, want 1 (no second bind)", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1", n)
	}
}

// TestBusySessionRejectsSecondPrompt: a second prompt while a turn is in
// flight is a stable conflict.
func TestBusySessionRejectsSecondPrompt(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "busy", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	if _, err := a.Prompt(bg(), m, text("second")); !isBusy(err) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
}
