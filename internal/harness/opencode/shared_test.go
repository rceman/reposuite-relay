package opencode

import (
	"errors"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestSharedRuntimeHostsManySessions: one runtime generation, many native
// sessions — one process, one initialize, one session/new each.
func TestSharedRuntimeHostsManySessions(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	cwd := t.TempDir()
	sessions := make([]*session.Managed, 5)
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
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 5 {
		t.Fatalf("session/new calls = %d, want 5", n)
	}
	if len(ids) != 5 {
		t.Fatalf("distinct native ids = %d, want 5", len(ids))
	}
	// One shared runtime process for all five.
	if e.Supervisor.Len() != 1 {
		t.Fatalf("runtime count = %d, want 1", e.Supervisor.Len())
	}
	pids := map[int]bool{}
	for _, m := range sessions {
		v, ok := e.Supervisor.View(m.Session.ID)
		if !ok {
			t.Fatalf("session %s has no runtime view", m.Session.Key)
		}
		pids[v.PID] = true
	}
	if len(pids) != 1 {
		t.Fatalf("distinct runtime PIDs = %d, want 1", len(pids))
	}
}

// TestDeletingOneSessionKeepsSiblingsAndRuntime: deleting a RelaySession
// detaches it and leaves the shared runtime serving the others.
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
		t.Fatalf("sibling runtime view = %+v ok=%v, want pid %d", v, ok, pid)
	}
	// The surviving session still works on the same runtime.
	res, err := a.Prompt(bg(), first, text("three"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RuntimeID == "" || res.RuntimeID == RuntimeKey {
		t.Fatalf("runtimeId %q must be the actual generation id, not the key", res.RuntimeID)
	}
	if v := mustView(t, e, first.Session.ID); v.RuntimeID != res.RuntimeID {
		t.Fatalf("runtimeId %q != bound generation %q", res.RuntimeID, v.RuntimeID)
	}
	e.WaitDurable(first.Session.ID, api.EventMessageAgentCompleted)
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 1 {
		t.Fatalf("initialize calls = %d, want 1 (runtime survived)", n)
	}
}

func mustView(t *testing.T, e *harnessenv.Env, sessionID string) runtime.View {
	t.Helper()
	v, ok := e.Supervisor.View(sessionID)
	if !ok {
		t.Fatalf("no runtime view for %s", sessionID)
	}
	return v
}

// TestPermissionIsAutoApprovedAndNeverBecomesInput: approvals are answered by
// policy and never surface as Relay requested-input.
func TestPermissionIsAutoApprovedAndNeverBecomesInput(t *testing.T) {
	e, a := newEnv(t, acp.FakePermission)
	m := newSession(t, e, a, "perm", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("write")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	joined := ""
	for _, line := range acp.ReadFakeEvents(e.ACPState) {
		joined += line + "\n"
	}
	if !contains(joined, "permission\tselected:always") {
		t.Fatalf("events = %v", acp.ReadFakeEvents(e.ACPState))
	}
	for _, typ := range e.DurableTypes(m.Session.ID) {
		if typ == api.EventInputRequested {
			t.Fatal("an approval must never become a durable input.requested")
		}
	}
}

// TestUnclassifiablePermissionFailsClosed: a request with no recognizable
// allow option is answered "cancelled".
func TestUnclassifiablePermissionFailsClosed(t *testing.T) {
	e, a := newEnv(t, acp.FakePermissionUnknown)
	m := newSession(t, e, a, "perm-unknown", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("write")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	joined := ""
	for _, line := range acp.ReadFakeEvents(e.ACPState) {
		joined += line + "\n"
	}
	if !contains(joined, "permission\tcancelled") {
		t.Fatalf("events = %v", acp.ReadFakeEvents(e.ACPState))
	}
}

// TestCancelIsNativeAndTerminal: cancel interrupts through session/cancel and
// the terminal state is the runtime's own "cancelled" stop reason.
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
	if n := acp.CountFakeEvents(e.ACPState, "cancel"); n != 1 {
		t.Fatalf("cancel notifications = %d, want 1", n)
	}
	harnessenv.WaitFor(t, "idle after cancel", func() bool { return turnSettled(e, a, m.Session.ID) })
	// A second cancel with no turn is a stable conflict.
	if err := a.Cancel(bg(), m); !isNoActiveTurn(err) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
	if e.Supervisor.Activity(m.Session.ID) != "idle" {
		t.Fatalf("activity = %q after cancel", e.Supervisor.Activity(m.Session.ID))
	}
}

// TestStreamingDeltasAndMetricsAreTransient: chunk deltas and metrics updates
// reach live subscribers without entering the durable transcript.
func TestStreamingDeltasAndMetricsAreTransient(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "stream", t.TempDir())
	es, ok := e.Broker.Get(m.Session.ID)
	if !ok {
		t.Fatal("no event state")
	}
	sub, err := es.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Unsubscribe(sub)
	if _, err := a.Prompt(bg(), m, text("stream me")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	// The usage merge runs on the completion worker after the terminal
	// durable record — wait for the projection itself before asserting.
	metrics := waitMetrics(t, a, m.Session.ID, func(mm *harness.SessionMetrics) bool {
		return mm.InputTokens != nil && *mm.InputTokens == 10
	})
	seen := collectEvents(t, sub)
	if seen[api.EventMessageAgentDelta] == 0 {
		t.Fatal("no transient message.agent.delta events")
	}
	if seen[api.EventMetricsUpdated] == 0 {
		t.Fatal("no transient metrics.updated events")
	}
	for _, typ := range e.DurableTypes(m.Session.ID) {
		if typ == api.EventMessageAgentDelta || typ == api.EventMetricsUpdated {
			t.Fatalf("%s must not be persisted", typ)
		}
	}
	if metrics.OutputTokens == nil || *metrics.OutputTokens != 5 {
		t.Fatalf("output tokens = %+v", metrics.OutputTokens)
	}
	if metrics.ReasoningTokens == nil || *metrics.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %+v", metrics.ReasoningTokens)
	}
	if metrics.ContextUsed == nil || *metrics.ContextUsed != 1234 {
		t.Fatalf("context used = %+v", metrics.ContextUsed)
	}
	if metrics.ContextLimit == nil || *metrics.ContextLimit != 200000 {
		t.Fatalf("context limit = %+v", metrics.ContextLimit)
	}
}

// collectEvents drains a subscription's replay + live queue.
func collectEvents(t *testing.T, sub *events.Subscription) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, ev := range sub.Replay {
		counts[ev.Type]++
	}
	for {
		select {
		case ev := <-sub.Events():
			counts[ev.Type]++
			continue
		default:
		}
		return counts
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// TestInFlightTurnBlocksRuntimeSleep: an active turn is a sleep blocker, so an
// idle-sleep sweep cannot tear the runtime out from under it.
func TestInFlightTurnBlocksRuntimeSleep(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "sleep-block", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	err := e.Supervisor.StopIfIdle(RuntimeKey)
	if !errors.Is(err, runtime.ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle err = %v, want ErrRuntimeBusy while a turn is in flight", err)
	}
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if err := e.Supervisor.StopIfIdle(RuntimeKey); err != nil {
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
