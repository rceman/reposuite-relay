package devin

import (
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestRuntimeDeathFailsTurnDurablyAndGoesCold.
func TestRuntimeDeathFailsTurnDurablyAndGoesCold(t *testing.T) {
	e, a := newEnv(t, acp.FakeDieOnPrompt)
	m := newSession(t, e, a, "die", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("die")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnFailed)
	e.WaitDurable(m.Session.ID, api.EventRuntimeExited)
	harnessenv.WaitFor(t, "COLD", func() bool { return !a.State(m.Session.ID).Live })
	if got := e.Reload(m.Session.ID); got.State != session.StateIdle || got.NativeSessionID != "" {
		t.Fatalf("session after death = %+v (an unproven slug must not persist)", got)
	}
	if e.Supervisor.Activity(m.Session.ID) != "idle" {
		t.Fatalf("activity = %q", e.Supervisor.Activity(m.Session.ID))
	}
}

// TestCancelIsNativeAndTerminal.
func TestCancelIsNativeAndTerminal(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "cancel", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
	if e.DurablePayload(m.Session.ID, api.EventMessageAgentCompleted) != nil {
		t.Fatal("a cancelled turn must not publish a completed message")
	}
	harnessenv.WaitFor(t, "idle after cancel", func() bool { return turnSettled(e, a, m.Session.ID) })
	if err := a.Cancel(bg(), m); !isNoActiveTurn(err) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

// TestBusySessionRejectsSecondPrompt.
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

// TestModelsGetSeparateRuntimes: two sessions with different models never share
// a process; two with the same model do.
func TestModelsGetSeparateRuntimes(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	cwd := t.TempDir()
	opus := newSession(t, e, a, "opus", cwd)
	opus.MetaMu.Lock()
	opus.Session.Model = "opus"
	opus.MetaMu.Unlock()
	sonnet := newSession(t, e, a, "sonnet", cwd)
	sonnet.MetaMu.Lock()
	sonnet.Session.Model = "sonnet"
	sonnet.MetaMu.Unlock()
	otherOpus := newSession(t, e, a, "opus2", cwd)
	otherOpus.MetaMu.Lock()
	otherOpus.Session.Model = "opus"
	otherOpus.MetaMu.Unlock()

	for _, m := range []*session.Managed{opus, sonnet, otherOpus} {
		if _, err := a.Prompt(bg(), m, text("hi")); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []*session.Managed{opus, sonnet, otherOpus} {
		e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	}
	if e.Supervisor.Len() != 2 {
		t.Fatalf("runtime count = %d, want 2 (one per distinct model)", e.Supervisor.Len())
	}
	opusView := mustView(t, e, opus.Session.ID)
	otherView := mustView(t, e, otherOpus.Session.ID)
	sonnetView := mustView(t, e, sonnet.Session.ID)
	if opusView.PID != otherView.PID {
		t.Fatal("sessions with the same model must share one runtime")
	}
	if opusView.PID == sonnetView.PID {
		t.Fatal("sessions with different models must not share a runtime")
	}
}

func mustView(t *testing.T, e *harnessenv.Env, sessionID string) debugView {
	t.Helper()
	v, ok := e.Supervisor.View(sessionID)
	if !ok {
		t.Fatalf("no runtime view for %s", sessionID)
	}
	return debugView{
		PID: v.PID,
		Key: v.Kind,
	}
}

type debugView struct {
	PID int
	Key string
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
