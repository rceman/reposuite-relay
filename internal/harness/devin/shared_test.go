package devin

import (
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
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
