package devin

import (
	"encoding/json"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
)

// TestRuntimeIDCorrelatesAcrossEventsAndGenerations: the supervisor key
// ("devin/acp/<model>") and Runtime.ID (one process generation's ephemeral
// identity) are different things; every public surface reports the generation
// id. A new generation gets a new id while the proven slug stays exact.
func TestRuntimeIDCorrelatesAcrossEventsAndGenerations(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "rtid", t.TempDir())

	if got := e.Reload(m.Session.ID).Generation; got != 0 {
		t.Fatalf("create generation = %d, want 0", got)
	}
	res1, err := a.Prompt(bg(), m, text("one"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	rt1 := res1.RuntimeID
	if rt1 == "" || rt1 == RuntimeKeyFor("") {
		t.Fatalf("prompt runtimeId %q must be a generation id, not the key", rt1)
	}
	if v, ok := e.Supervisor.View(m.Session.ID); !ok || v.RuntimeID != rt1 {
		t.Fatalf("status view %+v != prompt runtimeId %q", v, rt1)
	}
	var started api.HarnessStartedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventHarnessStarted), &started); err != nil {
		t.Fatal(err)
	}
	if started.RuntimeID != rt1 {
		t.Fatalf("harness.started runtimeId %q != %q", started.RuntimeID, rt1)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 1 {
		t.Fatalf("first bind generation = %d, want 1", got)
	}

	// Same-runtime second prompt: same generation id, no bump.
	res2, err := a.Prompt(bg(), m, text("two"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if res2.RuntimeID != rt1 {
		t.Fatalf("same-generation prompt runtimeId %q != %q", res2.RuntimeID, rt1)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 1 {
		t.Fatalf("same-runtime generation = %d, want 1", got)
	}

	// The generation dies: runtime.exited names the same generation id.
	if err := e.Supervisor.Stop(RuntimeKeyFor("")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "runtime.exited", func() bool {
		return e.DurablePayload(m.Session.ID, api.EventRuntimeExited) != nil
	})
	var exited api.RuntimeExitedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventRuntimeExited), &exited); err != nil {
		t.Fatal(err)
	}
	if exited.RuntimeID != rt1 {
		t.Fatalf("runtime.exited runtimeId %q != %q", exited.RuntimeID, rt1)
	}

	// A cold resume enters a NEW generation: a different runtime id, the
	// exact same proven slug, and the binding bumps the generation.
	res3, err := a.Prompt(bg(), m, text("three"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if res3.RuntimeID == rt1 || res3.RuntimeID == "" {
		t.Fatalf("cold resume must bind a new generation, got runtimeId %q", res3.RuntimeID)
	}
	if res3.NativeSessionID != res1.NativeSessionID {
		t.Fatalf("native id = %q, want the exact proven %q", res3.NativeSessionID, res1.NativeSessionID)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 2 {
		t.Fatalf("cold resume generation = %d, want 2", got)
	}
}

// TestFailedFirstTurnLeavesBindingGeneration: a refused first turn fails, but
// the binding it entered still counts (Generation=1) while the unproven slug
// is never persisted (NativeSessionID="") — a binding existed, a resumable
// identity did not.
func TestFailedFirstTurnLeavesBindingGeneration(t *testing.T) {
	e, a := newEnv(t, acp.FakeFailTurn)
	m := newSession(t, e, a, "fail", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hi")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnFailed)
	got := e.Reload(m.Session.ID)
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (the binding existed)", got.Generation)
	}
	if got.NativeSessionID != "" {
		t.Fatalf("an unproven slug must not be persisted, got %q", got.NativeSessionID)
	}
	if st := a.State(m.Session.ID); !st.Live || st.Materialized {
		t.Fatalf("a refused turn keeps the generation live but unproven: %+v", st)
	}
}

// turnSettled reports whether a turn is fully done: no in-flight turn AND the
// supervisor sees the session idle. The stronger condition matters for
// mutations — clearing the in-flight marker happens before activity resets.
func turnSettled(e *harnessenv.Env, a *Adapter, sessionID string) bool {
	return !a.State(sessionID).TurnInFlight &&
		e.Supervisor.Activity(sessionID) == runtime.ActivityIdle
}
