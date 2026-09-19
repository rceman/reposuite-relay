package devin

import (
	"errors"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestSharedRuntimeHostsManySessions: one model → one runtime generation hosting
// many native sessions.
func TestSharedRuntimeHostsManySessions(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	cwd := t.TempDir()
	sessions := make([]*session.Managed, 4)
	ids := map[string]bool{}
	for i := range sessions {
		sessions[i] = newSession(t, e, a, "shared", cwd)
		res, err := a.Prompt(bg(), sessions[i], text("hi"))
		if err != nil {
			t.Fatal(err)
		}
		ids[res.NativeSessionID] = true
	}
	for i := range sessions {
		e.WaitDurable(sessions[i].Session.ID, api.EventMessageAgentCompleted)
	}
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 1 {
		t.Fatalf("initialize calls = %d, want 1 shared runtime", n)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 4 {
		t.Fatalf("session/new calls = %d, want 4", n)
	}
	if len(ids) != 4 {
		t.Fatalf("distinct native ids = %d, want 4", len(ids))
	}
	if e.Supervisor.Len() != 1 {
		t.Fatalf("runtime count = %d, want 1", e.Supervisor.Len())
	}
	pids := map[int]bool{}
	for _, m := range sessions {
		pids[mustView(t, e, m.Session.ID).PID] = true
	}
	if len(pids) != 1 {
		t.Fatalf("distinct runtime PIDs = %d, want 1", len(pids))
	}
}

// TestDeletingOneSessionKeepsSiblingsAndRuntime.
func TestDeletingOneSessionKeepsSiblingsAndRuntime(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	cwd := t.TempDir()
	first := newSession(t, e, a, "keep", cwd)
	second := newSession(t, e, a, "delete", cwd)
	if _, err := a.Prompt(bg(), first, text("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(bg(), second, text("two")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(first.Session.ID, api.EventMessageAgentCompleted)
	e.WaitDurable(second.Session.ID, api.EventMessageAgentCompleted)
	pid := mustView(t, e, first.Session.ID).PID

	a.StopSession(second)
	if e.Supervisor.Len() != 1 {
		t.Fatalf("runtime count = %d after session delete, want 1", e.Supervisor.Len())
	}
	if v, ok := e.Supervisor.View(first.Session.ID); !ok || v.PID != pid {
		t.Fatalf("sibling view = %+v ok=%v, want pid %d", v, ok, pid)
	}
	if _, err := a.Prompt(bg(), first, text("three")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(first.Session.ID, api.EventMessageAgentCompleted)
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 1 {
		t.Fatalf("initialize calls = %d, want 1 (runtime survived)", n)
	}
}

// TestStopSessionOnRuntimelessSessionIsSafe: deleting a COLD session must not
// disturb anything.
func TestStopSessionOnRuntimelessSessionIsSafe(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "cold-delete", t.TempDir())
	a.StopSession(m)
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("a deleted session must not keep a runtime binding")
	}
	if a.Metrics(m.Session.ID) != nil {
		t.Fatal("adapter state must be released")
	}
}

// TestInFlightTurnBlocksRuntimeSleep: an active turn is a sleep blocker.
func TestInFlightTurnBlocksRuntimeSleep(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "sleep-block", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	if err := e.Supervisor.StopIfIdle(RuntimeKeyFor("")); !errors.Is(err, runtime.ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle err = %v, want ErrRuntimeBusy while a turn is in flight", err)
	}
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if err := e.Supervisor.StopIfIdle(RuntimeKeyFor("")); err != nil {
		t.Fatalf("StopIfIdle after the turn = %v, want success", err)
	}
}

// waitMetrics waits for the adapter's metrics projection to satisfy pred.
// Usage metrics are merged by the turn-completion worker AFTER the terminal
// durable record is published, so waiting on message.agent.completed alone
// does not order against the projection — wait for the value itself.
func waitMetrics(t *testing.T, a *Adapter, sessionID string, pred func(*harness.SessionMetrics) bool) *harness.SessionMetrics {
	t.Helper()
	var got *harness.SessionMetrics
	harnessenv.WaitFor(t, "metrics projection", func() bool {
		got = a.Metrics(sessionID)
		return got != nil && pred(got)
	})
	return got
}
