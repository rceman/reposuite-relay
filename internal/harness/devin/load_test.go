// session/load contract tests: the no-echo result shape observed from
// real devin acp 3000.11.1, hard-failure non-substitution, and the
// orphan-generation reap on bind failure.
package devin

import (
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
)

// TestFailedExactLoadNeverSubstitutes: a native load failure is a hard failure.
func TestFailedExactLoadNeverSubstitutes(t *testing.T) {
	e, a := newEnv(t, acp.FakeLoadError)
	m := newSession(t, e, a, "lost", t.TempDir())
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "happy-lark"
	m.MetaMu.Unlock()
	a.Track(m)
	if _, err := a.Prompt(bg(), m, text("hello")); !isNativeSessionLost(err) {
		t.Fatalf("err = %v, want NATIVE_SESSION_LOST", err)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 0 {
		t.Fatalf("session/new calls = %d, want 0 (no substitution)", n)
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed record")
	}
	if got := e.Reload(m.Session.ID).NativeSessionID; got != "happy-lark" {
		t.Fatalf("persisted slug = %q, want the unchanged exact identity", got)
	}
	// The generation spawned for the failed bind must be reaped — an
	// orphaned devin acp process holds the provider session lock and
	// poisons every later session/load (real dogfood finding).
	harnessenv.WaitFor(t, "orphan generation reaped", func() bool {
		return e.Supervisor.Len() == 0
	})
}

// TestLoadWithoutIdentityEcho: a provider that does not echo sessionId
// in the session/load result (devin acp 3000.11.1) still resumes the
// exact requested identity — the request named it.
func TestLoadWithoutIdentityEcho(t *testing.T) {
	e, a := newEnv(t, acp.FakeLoadNoEcho)
	m := newSession(t, e, a, "noecho", t.TempDir())
	first, err := a.Prompt(bg(), m, text("first"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventNativeSession)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if err := e.Supervisor.Stop(RuntimeKeyFor("")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "COLD after runtime stop", func() bool { return !a.State(m.Session.ID).Live })

	second, err := a.Prompt(bg(), m, text("second"))
	if err != nil {
		t.Fatalf("no-echo session/load must resume: %v", err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if second.NativeSessionID != first.NativeSessionID {
		t.Fatalf("resumed %q, want the exact %q", second.NativeSessionID, first.NativeSessionID)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 2 {
		t.Fatalf("generation = %d, want 2", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != 1 {
		t.Fatalf("session/load calls = %d, want 1", n)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (no substitution)", n)
	}
}
