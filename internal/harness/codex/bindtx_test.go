package codex

import (
	"context"
	"errors"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestMaterializeFailureRollsBackBind: when the durable commit fails AFTER a
// successful Supervisor.Bind, the binding is rolled back synchronously — no
// phantom supervisor binding, no thread routing, no generation bump, no
// events — and the shared app-server keeps running.
func TestMaterializeFailureRollsBackBind(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("commitfail", t.TempDir())

	// Fail exactly the bind commit — every other durable write (state
	// transitions, events) goes through normally.
	orig := e.adapter.deps.Materialize
	failed := false
	e.adapter.deps.Materialize = func(mm *session.Managed, upd harness.SessionUpdate) error {
		if !failed && upd.BumpGeneration {
			failed = true
			return errors.New("injected durable commit failure")
		}
		return orig(mm, upd)
	}

	_, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "hello"})
	if err == nil {
		t.Fatal("prompt must fail when the durable commit fails")
	}
	if !failed {
		t.Fatal("the injected commit failure never fired")
	}

	got := e.reload(m.Session.ID)
	if got.Generation != 0 {
		t.Fatalf("generation = %d, want 0 — the commit failed", got.Generation)
	}
	if got.NativeSessionID != "" {
		t.Fatalf("nativeSessionId = %q, want empty — a provisional thread is never persisted", got.NativeSessionID)
	}
	if _, ok := e.sup.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding survived rollback")
	}
	st := e.adapter.state(m.Session.ID)
	e.adapter.mu.Lock()
	live := st.nativeID != "" || st.liveKey != "" || st.current != nil
	routes := len(e.adapter.threads)
	e.adapter.mu.Unlock()
	if live {
		t.Fatal("adapter live binding survived rollback")
	}
	if routes != 0 {
		t.Fatal("thread routing entry survived rollback")
	}
	if e.durablePayload(m.Session.ID, api.EventMessageUser) == nil {
		t.Fatal("accepted prompt lost its durable message.user")
	}
	if e.durablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}
	if e.durablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a failed commit")
	}

	// The shared app-server stays healthy: the next prompt binds into the
	// same generation and the durable commit succeeds.
	res, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "again"})
	if err != nil {
		t.Fatalf("prompt after rollback: %v", err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	got = e.reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d after recovery, want 1", got.Generation)
	}
	if res.RuntimeID == "" {
		t.Fatal("no runtimeId on the recovered prompt")
	}
}

// TestRuntimeDeathMidBindRecovers: a generation death landing inside the
// commit window — after Supervisor.Bind but before routing existed — must
// not leave a phantom binding, a phantom start event, or stale routing.
// The durable commit stands (the binding WAS committed before its
// generation died), and the next prompt binds into a fresh generation.
func TestRuntimeDeathMidBindRecovers(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("midbind", t.TempDir())

	// Kill the generation synchronously inside the bind commit — after
	// Supervisor.Bind, before routing exists — so the death path cannot
	// see this session's binding.
	orig := e.adapter.deps.Materialize
	killed := false
	e.adapter.deps.Materialize = func(mm *session.Managed, upd harness.SessionUpdate) error {
		if !killed && upd.BumpGeneration {
			killed = true
			if err := e.sup.Stop(RuntimeKey); err != nil {
				return err
			}
		}
		return orig(mm, upd)
	}

	_, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "hello"})
	if err == nil {
		t.Fatal("prompt must fail when its generation dies mid-bind")
	}
	if !killed {
		t.Fatal("the mid-bind kill never fired")
	}

	// The commit stands durably: generation 1. The provisional thread
	// died with its process, so the durable identity stays empty — a
	// committed binding without a proven resumable identity.
	got := e.reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1 — the binding committed before the death", got.Generation)
	}
	if got.NativeSessionID != "" {
		t.Fatalf("nativeSessionId = %q, want empty — the provisional thread is never persisted", got.NativeSessionID)
	}
	if _, ok := e.sup.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding on a dead generation")
	}
	st := e.adapter.state(m.Session.ID)
	e.adapter.mu.Lock()
	live := st.liveKey != "" || st.nativeID != ""
	routes := len(e.adapter.threads)
	e.adapter.mu.Unlock()
	if live {
		t.Fatal("stale adapter routing on a dead generation")
	}
	if routes != 0 {
		t.Fatal("thread routing entry survived a dead generation")
	}
	if e.durablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a generation that died mid-bind")
	}
	if e.durablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}

	// Recovery: the next prompt binds into a fresh generation with a new
	// thread — the provisional thread was never resumable.
	res, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "again"})
	if err != nil {
		t.Fatalf("prompt after mid-bind death: %v", err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	got = e.reload(m.Session.ID)
	if got.Generation != 2 {
		t.Fatalf("generation = %d after recovery, want 2", got.Generation)
	}
	if res.RuntimeID == "" {
		t.Fatal("no runtimeId on the recovered prompt")
	}
}
