package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
)

// TestRuntimeIDCorrelatesAcrossEventsAndGenerations: the supervisor key
// ("codex/default") and Runtime.ID (one process generation's ephemeral
// identity) are different things; every public surface reports the generation
// id. A new generation gets a new id while the native thread id stays exact.
func TestRuntimeIDCorrelatesAcrossEventsAndGenerations(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("rtid", t.TempDir())

	if got := e.reload(m.Session.ID).Generation; got != 0 {
		t.Fatalf("create generation = %d, want 0", got)
	}
	res1, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	waitFor(t, "turn settled", func() bool { return e.adapter.Idle(m.Session.ID) })
	rt1 := res1.RuntimeID
	if rt1 == "" || rt1 == RuntimeKey {
		t.Fatalf("prompt runtimeId %q must be a generation id, not the key", rt1)
	}
	if v, ok := e.sup.View(m.Session.ID); !ok || v.RuntimeID != rt1 {
		t.Fatalf("status view %+v != prompt runtimeId %q", v, rt1)
	}
	var started api.HarnessStartedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventHarnessStarted), &started); err != nil {
		t.Fatal(err)
	}
	if started.RuntimeID != rt1 {
		t.Fatalf("harness.started runtimeId %q != %q", started.RuntimeID, rt1)
	}
	if got := e.reload(m.Session.ID).Generation; got != 1 {
		t.Fatalf("first bind generation = %d, want 1", got)
	}

	// Same-runtime second prompt: same generation id, no bump.
	res2, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	waitFor(t, "turn settled", func() bool { return e.adapter.Idle(m.Session.ID) })
	if res2.RuntimeID != rt1 {
		t.Fatalf("same-generation prompt runtimeId %q != %q", res2.RuntimeID, rt1)
	}
	if got := e.reload(m.Session.ID).Generation; got != 1 {
		t.Fatalf("same-runtime generation = %d, want 1", got)
	}

	// The generation dies: runtime.exited names the same generation id.
	if err := e.sup.Stop(RuntimeKey); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runtime.exited", func() bool {
		return e.durablePayload(m.Session.ID, api.EventRuntimeExited) != nil
	})
	var exited api.RuntimeExitedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventRuntimeExited), &exited); err != nil {
		t.Fatal(err)
	}
	if exited.RuntimeID != rt1 {
		t.Fatalf("runtime.exited runtimeId %q != %q", exited.RuntimeID, rt1)
	}

	// A cold resume enters a NEW generation: a different runtime id, the
	// exact same thread id, and the binding bumps the generation.
	res3, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "three"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	waitFor(t, "turn settled", func() bool { return e.adapter.Idle(m.Session.ID) })
	if res3.RuntimeID == rt1 || res3.RuntimeID == "" {
		t.Fatalf("cold resume must bind a new generation, got runtimeId %q", res3.RuntimeID)
	}
	if res3.NativeSessionID != res1.NativeSessionID {
		t.Fatalf("native id = %q, want the exact %q", res3.NativeSessionID, res1.NativeSessionID)
	}
	if got := e.reload(m.Session.ID).Generation; got != 2 {
		t.Fatalf("cold resume generation = %d, want 2", got)
	}
}
