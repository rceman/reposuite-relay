package opencode

import (
	"encoding/json"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
)

// TestRuntimeIDCorrelatesAcrossEventsAndGenerations: Runtime.Key (the stable
// supervisor grouping key "opencode/acp") and Runtime.ID (the ephemeral
// identity of one process generation) are different things, and every public
// surface reports the GENERATION id: prompt response, status view,
// harness.started, runtime.exited. A new generation gets a new id while the
// native session id stays exact.
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
	if rt1 == "" || rt1 == RuntimeKey {
		t.Fatalf("prompt runtimeId %q must be a generation id, not the key", rt1)
	}
	if v := mustView(t, e, m.Session.ID); v.RuntimeID != rt1 {
		t.Fatalf("status runtimeId %q != prompt runtimeId %q", v.RuntimeID, rt1)
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
	if err := e.Supervisor.Stop(RuntimeKey); err != nil {
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
	// exact same native session id, and the binding bumps the generation.
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
		t.Fatalf("native id = %q, want the exact %q", res3.NativeSessionID, res1.NativeSessionID)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 2 {
		t.Fatalf("cold resume generation = %d, want 2", got)
	}
}

// TestFailedFirstTurnLeavesBindingGeneration: a refused first turn fails but
// the binding it entered still counts — the runtime stays alive and the
// generation reflects the binding even though the turn produced no success.
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
	// OpenCode materializes the ses_* identity at session/new, so it is
	// durable even though the turn failed.
	if got.NativeSessionID == "" {
		t.Fatal("the native identity from session/new must be durable")
	}
	if !a.State(m.Session.ID).Live {
		t.Fatal("a refused turn must not kill the runtime generation")
	}
}
