package devin

import (
	"encoding/json"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestConfigModelAppliesToNextGeneration: Devin's model is process-level, so
// a warm model change migrates the session to a new model-partitioned
// generation — the old handle is never reused and the old shared runtime is
// left running.
func TestConfigModelAppliesToNextGeneration(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "model", t.TempDir())
	liveFirstTurn(t, e, a, m)
	before := acp.CountFakeEvents(e.ACPState, "set_config_option")
	provenSlug := e.Reload(m.Session.ID).NativeSessionID
	if provenSlug == "" {
		t.Fatal("no proven slug after a completed turn")
	}
	loadsBefore := acp.CountFakeEvents(e.ACPState, "session/load")
	newsBefore := acp.CountFakeEvents(e.ACPState, "session/new")

	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if got := e.Reload(m.Session.ID).Model; got != "opus" {
		t.Fatalf("model = %q", got)
	}
	var cfg api.ConfigChangedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventConfigChanged), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "opus" {
		t.Fatalf("session.config = %+v", cfg)
	}
	// No live model mutation was attempted or claimed.
	if after := acp.CountFakeEvents(e.ACPState, "set_config_option"); after != before {
		t.Fatalf("native mutations went %d -> %d", before, after)
	}
	// The old handle is gone; the OLD shared runtime keeps running.
	if a.State(m.Session.ID).Live {
		t.Fatal("the incompatible live handle must be detached")
	}
	if _, ok := e.Supervisor.Get(RuntimeKeyFor("")); !ok {
		t.Fatal("the old shared runtime must stay alive")
	}

	// The next prompt migrates: the SAME proven slug is loaded into a
	// generation partitioned by the new model — never a session/new.
	res, err := a.Prompt(bg(), m, text("two"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if res.NativeSessionID != provenSlug {
		t.Fatalf("native id = %q, want the exact proven %q", res.NativeSessionID, provenSlug)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != loadsBefore+1 {
		t.Fatalf("session/load calls = %d, want %d (exact slug, new generation)", n, loadsBefore+1)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != newsBefore {
		t.Fatalf("session/new calls = %d, want %d (never a substitution)", n, newsBefore)
	}
	view, ok := e.Supervisor.View(m.Session.ID)
	if !ok || view.Kind != session.HarnessDevin {
		t.Fatalf("runtime view = %+v ok=%v", view, ok)
	}
	if _, ok := e.Supervisor.Get(RuntimeKeyFor("opus")); !ok {
		t.Fatal("the recorded model must select the model-partitioned runtime")
	}
	// Both generations exist: the old shared one and the new model's.
	if e.Supervisor.Len() != 2 {
		t.Fatalf("runtime count = %d, want 2 (old shared + new model generation)", e.Supervisor.Len())
	}
}

// TestConfigColdSessionDoesNotWakeRuntime: a COLD process-model change is a
// durable preference that wakes nothing; the next process is launched with
// that model (the model-partitioned runtime key).
func TestConfigColdSessionDoesNotWakeRuntime(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "cold-config", t.TempDir())
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "opus", Mode: "ask"}); err != nil {
		t.Fatal(err)
	}
	got := e.Reload(m.Session.ID)
	if got.Model != "opus" || got.Mode != "ask" {
		t.Fatalf("recorded config = %+v", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 0 {
		t.Fatalf("a cold config change must not wake a runtime (initialize = %d)", n)
	}
	if e.Supervisor.Len() != 0 {
		t.Fatalf("runtime count = %d, want 0", e.Supervisor.Len())
	}
	// The next process really is launched for the desired model.
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if _, ok := e.Supervisor.Get(RuntimeKeyFor("opus")); !ok {
		t.Fatal("the desired model must select the model-partitioned runtime")
	}
}

// TestPromptOverridesAreRejected: per-turn model/effort overrides have no
// verified Devin equivalent, so they are rejected BEFORE the prompt is
// accepted — no durable message.user, no native prompt.
func TestPromptOverridesAreRejected(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "override", t.TempDir())
	for _, cmd := range []harness.PromptCommand{
		{Text: "hi", Model: "opus"},
		{Text: "hi", Effort: "high"},
	} {
		if _, err := a.Prompt(bg(), m, cmd); !isUnsupported(err) {
			t.Fatalf("err = %v, want ErrUnsupported for %+v", err, cmd)
		}
	}
	if e.DurablePayload(m.Session.ID, api.EventMessageUser) != nil {
		t.Fatal("a rejected prompt must not publish message.user")
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) != nil {
		t.Fatal("a rejected prompt is a refusal, not a failed turn")
	}
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 0 {
		t.Fatalf("a rejected prompt must not wake a runtime (%d)", n)
	}
}

// TestConfigDuringActiveTurnIsBusy: a live config change while a turn is in
// flight is rejected with SESSION_BUSY — no mid-turn mutation or migration.
func TestConfigDuringActiveTurnIsBusy(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "busy-cfg", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "opus"}); !isBusy(err) {
		t.Fatalf("err = %v, want ErrBusy during an in-flight turn", err)
	}
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return !a.State(m.Session.ID).TurnInFlight })
	if got := e.Reload(m.Session.ID).Model; got == "opus" {
		t.Fatal("a rejected config change must not be recorded")
	}
	// The session never migrated: it still lives on the same generation.
	if st := a.State(m.Session.ID); !st.Live || st.LiveRuntimeKey != RuntimeKeyFor("") {
		t.Fatalf("state after rejected migration = %+v", st)
	}
}
