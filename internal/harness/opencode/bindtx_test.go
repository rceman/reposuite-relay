package opencode

import (
	"errors"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestBindFailureLeavesNoCommittedBinding: when Supervisor.Bind fails after
// session/new and desired-config application, the transaction must leave
// nothing committed — no phantom generation, binding, routing, or events —
// and the shared runtime keeps running.
func TestBindFailureLeavesNoCommittedBinding(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "bindfail", t.TempDir())

	// A live agent the supervisor never registered: session/new succeeds
	// natively, but Supervisor.Bind can only fail (no runtime generation is
	// registered for the key) — a deterministic bind failure.
	cmdFn := harnessenv.FakeACPCommand(t, acp.FakeVendorOpenCode, acp.FakeHappy, e.ACPState)
	cmd, err := cmdFn()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := acp.StartServer(cmd, m.Snapshot().Cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	srv.Configure(acp.ServerConfig{
		Client:   acp.ClientInfo{Name: "reposuite-relay-test"},
		Approve:  a.approve,
		OnUpdate: a.onUpdate,
	})
	a.wg.Add(1)
	go func() {
		_ = srv.Serve()
		a.wg.Done()
	}()
	if _, err := srv.Initialize(bg(), acp.ClientInfo{Name: "reposuite-relay-test"}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.servers[RuntimeKey] = srv
	a.mu.Unlock()

	if _, err := a.Prompt(bg(), m, text("hello")); err == nil {
		t.Fatal("prompt must fail when Supervisor.Bind fails")
	}

	// The native session was prepared (session/new ran), then rolled back:
	// nothing durable may reflect a successful binding.
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (prepared then rolled back)", n)
	}
	got := e.Reload(m.Session.ID)
	if got.Generation != 0 {
		t.Fatalf("generation = %d, want 0 — Bind never committed", got.Generation)
	}
	if got.NativeSessionID != "" {
		t.Fatalf("nativeSessionId = %q, want empty", got.NativeSessionID)
	}
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding survived the failure")
	}
	st := a.state(m.Session.ID)
	a.mu.Lock()
	live := st.handle != nil || st.srv != nil || st.nativeID != ""
	routes := len(a.byNative)
	a.mu.Unlock()
	if live {
		t.Fatal("adapter live binding survived the bind failure")
	}
	if routes != 0 {
		t.Fatal("byNative routing entry survived the bind failure")
	}
	// The prompt was accepted (message.user + turn.failed), but no
	// session.native or harness.started may advertise a binding that never
	// committed.
	if e.DurablePayload(m.Session.ID, api.EventMessageUser) == nil {
		t.Fatal("accepted prompt lost its durable message.user")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}
	if e.DurablePayload(m.Session.ID, api.EventNativeSession) != nil {
		t.Fatal("session.native published for a binding that never committed")
	}
	if e.DurablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a binding that never committed")
	}
	// The shared runtime must not be killed by one session's failed
	// transaction.
	select {
	case <-srv.Wait():
		t.Fatal("the shared runtime was killed by the failed bind")
	default:
	}
}

// TestMaterializeFailureRollsBackBind: when the durable commit fails AFTER a
// successful Supervisor.Bind, the binding is rolled back synchronously —
// no phantom supervisor binding, no routing, no generation, no events.
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
		t.Fatalf("nativeSessionId = %q, want empty", got.NativeSessionID)
	}
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding survived rollback")
	}
	st := a.state(m.Session.ID)
	a.mu.Lock()
	live := st.handle != nil || st.srv != nil || st.nativeID != ""
	routes := len(a.byNative)
	a.mu.Unlock()
	if live {
		t.Fatal("adapter live binding survived rollback")
	}
	if routes != 0 {
		t.Fatal("byNative routing entry survived rollback")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the accepted prompt")
	}
	if e.DurablePayload(m.Session.ID, api.EventNativeSession) != nil {
		t.Fatal("session.native published for a failed commit")
	}
	if e.DurablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a failed commit")
	}

	// The shared runtime stays healthy: a fresh commit succeeds on the
	// next prompt into the same generation.
	res, err := a.Prompt(bg(), m, text("again"))
	if err != nil {
		t.Fatalf("prompt after rollback: %v", err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	got = e.Reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d after recovery, want 1", got.Generation)
	}
	if res.NativeSessionID != got.NativeSessionID {
		t.Fatalf("resumed id %q != durable %q", res.NativeSessionID, got.NativeSessionID)
	}
}

// TestRuntimeDeathMidBindRecovers: a generation death landing inside the
// commit window — after Supervisor.Bind but before routing existed — must
// not leave a phantom binding, a phantom start event, or a stale handle.
// The durable commit stands (the binding WAS committed before its
// generation died), and the next prompt resumes the exact ses_* into a
// fresh generation.
func TestRuntimeDeathMidBindRecovers(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "midbind", t.TempDir())

	// Kill the generation synchronously inside the bind commit — after
	// Supervisor.Bind, before routing exists — so the death path cannot
	// see this session's binding.
	orig := a.deps.Materialize
	killed := false
	a.deps.Materialize = func(mm *session.Managed, upd harness.SessionUpdate) error {
		if !killed && upd.BumpGeneration {
			killed = true
			if err := e.Supervisor.Stop(RuntimeKey); err != nil {
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

	// The commit stands durably: generation 1, ses_* persisted. No
	// phantom supervisor binding, no adapter routing on a dead server.
	got := e.Reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1 — the binding committed before the death", got.Generation)
	}
	nativeID := got.NativeSessionID
	if nativeID == "" {
		t.Fatal("the committed ses_* identity must survive the dead generation")
	}
	if _, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatal("phantom supervisor binding on a dead generation")
	}
	st := a.state(m.Session.ID)
	a.mu.Lock()
	live := st.handle != nil || st.srv != nil
	a.mu.Unlock()
	if live {
		t.Fatal("stale adapter routing on a dead generation")
	}
	if e.DurablePayload(m.Session.ID, api.EventHarnessStarted) != nil {
		t.Fatal("harness.started published for a generation that died mid-bind")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed for the failed submission")
	}

	// Recovery: the next prompt resumes the EXACT ses_* into a fresh
	// generation — never a substitute session/new.
	res, err := a.Prompt(bg(), m, text("again"))
	if err != nil {
		t.Fatalf("prompt after mid-bind death: %v", err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if res.NativeSessionID != nativeID {
		t.Fatalf("resumed %q, want the exact committed %q", res.NativeSessionID, nativeID)
	}
	got = e.Reload(m.Session.ID)
	if got.Generation != 2 {
		t.Fatalf("generation = %d after recovery, want 2", got.Generation)
	}
	if got.NativeSessionID != nativeID {
		t.Fatalf("durable identity changed: %q, want %q", got.NativeSessionID, nativeID)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (the first prepare only)", n)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != 1 {
		t.Fatalf("session/load calls = %d, want 1 — the committed identity must resume exactly", n)
	}
}
