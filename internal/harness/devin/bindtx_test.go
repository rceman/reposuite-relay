package devin

import (
	"errors"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestMaterializeFailureRollsBackBind: when the durable commit fails AFTER a
// successful Supervisor.Bind, the existing detach rollback must restore the
// pre-transaction state synchronously — no phantom supervisor binding, no
// live runtime key, no routing, no generation bump, no events.
func TestMaterializeFailureRollsBackBind(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "commitfail", t.TempDir())

	// Fail exactly the bind commit — every other durable write (state
	// transitions, events) goes through normally.
	orig := a.deps.Materialize
	failed := false
	a.deps.Materialize = func(mm *session.Managed, upd harness.SessionUpdate) error {
		if !failed && upd.BumpGeneration {
			failed = true
			return errors.New("injected durable commit failure")
		}
		return orig(mm, upd)
	}

	if _, err := a.Prompt(bg(), m, text("hello")); err == nil {
		t.Fatal("prompt must fail when the durable commit fails")
	}
	if !failed {
		t.Fatal("the injected commit failure never fired")
	}

	got := e.Reload(m.Session.ID)
	if got.Generation != 0 {
		t.Fatalf("generation = %d, want 0 — the commit failed", got.Generation)
	}
	if got.NativeSessionID != "" {
		t.Fatalf("nativeSessionId = %q, want empty — an unproven slug is never persisted", got.NativeSessionID)
	}
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding survived rollback")
	}
	st := a.state(m.Session.ID)
	a.mu.Lock()
	live := st.handle != nil || st.srv != nil || st.liveRuntimeKey != "" || st.nativeID != ""
	routes := len(a.byNative)
	a.mu.Unlock()
	if live {
		t.Fatal("adapter live binding survived rollback")
	}
	if routes != 0 {
		t.Fatal("byNative routing entry survived rollback")
	}
	if e.DurablePayload(m.Session.ID, api.EventMessageUser) == nil {
		t.Fatal("accepted prompt lost its durable message.user")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}
	if e.DurablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a failed commit")
	}

	// The shared runtime stays healthy: the next prompt binds into the
	// same model generation and the commit succeeds.
	res, err := a.Prompt(bg(), m, text("again"))
	if err != nil {
		t.Fatalf("prompt after rollback: %v", err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	got = e.Reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d after recovery, want 1", got.Generation)
	}
	if res.RuntimeID == "" {
		t.Fatal("no runtimeId on the recovered prompt")
	}
}

// TestRuntimeDeathMidBindRecovers: a generation death landing inside the
// commit window must not leave a phantom binding, a phantom start event,
// or a stale handle. The durable commit stands (the binding WAS
// committed before its generation died), and the next prompt binds into
// a fresh generation.
func TestRuntimeDeathMidBindRecovers(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "midbind", t.TempDir())

	// Kill the generation synchronously inside the bind commit.
	orig := a.deps.Materialize
	killed := false
	a.deps.Materialize = func(mm *session.Managed, upd harness.SessionUpdate) error {
		if !killed && upd.BumpGeneration {
			killed = true
			if err := e.Supervisor.Stop(RuntimeKeyFor(m.Snapshot().Model)); err != nil {
				return err
			}
		}
		return orig(mm, upd)
	}

	if _, err := a.Prompt(bg(), m, text("hello")); err == nil {
		t.Fatal("prompt must fail when its generation dies mid-bind")
	}
	if !killed {
		t.Fatal("the mid-bind kill never fired")
	}

	got := e.Reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1 — the binding committed before the death", got.Generation)
	}
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding on a dead generation")
	}
	st := a.state(m.Session.ID)
	a.mu.Lock()
	live := st.handle != nil || st.srv != nil || st.liveRuntimeKey != ""
	routes := len(a.byNative)
	a.mu.Unlock()
	if live {
		t.Fatal("stale adapter routing on a dead generation")
	}
	if routes != 0 {
		t.Fatal("byNative routing entry survived the dead generation")
	}
	if e.DurablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a generation that died mid-bind")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}

	// Recovery: the next prompt binds into a fresh generation. The
	// unproven slug died with its generation, so a new native session is
	// created — honest, because a zero-turn Devin session is never
	// resumable.
	res, err := a.Prompt(bg(), m, text("again"))
	if err != nil {
		t.Fatalf("prompt after mid-bind death: %v", err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	got = e.Reload(m.Session.ID)
	if got.Generation != 2 {
		t.Fatalf("generation = %d after recovery, want 2", got.Generation)
	}
	if res.RuntimeID == "" {
		t.Fatal("no runtimeId on the recovered prompt")
	}
}
