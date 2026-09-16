package codex

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
)

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

// TestMalformedNativeIdentityFailsClosed: a durable native identity that
// is not a plausible thread id is never sent to the harness — the prompt
// fails with ErrNativeSessionLost instead of resuming something else.
func TestMalformedNativeIdentityFailsClosed(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("malformed", t.TempDir())
	m.Session.NativeSessionID = "not a thread id\n"
	if err := e.store.Save(m.Session); err != nil {
		t.Fatal(err)
	}
	e.adapter.Track(m)
	_, err := e.adapter.Prompt(context.Background(), m, "hello", "", "")
	if !errors.Is(err, ErrNativeSessionLost) {
		t.Fatalf("err = %v, want ErrNativeSessionLost", err)
	}
	if e.sup.Len() != 0 {
		t.Fatal("a malformed identity must not spawn a runtime")
	}
	if p := e.durablePayload(m.Session.ID, api.EventTurnFailed); p == nil {
		t.Fatal("no durable turn.failed record")
	}
	if got := e.reload(m.Session.ID); got.NativeSessionID != "not a thread id\n" {
		t.Fatalf("durable identity was rewritten: %q", got.NativeSessionID)
	}
}
