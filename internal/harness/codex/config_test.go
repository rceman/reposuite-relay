package codex

import (
	"context"
	"os"
	"testing"

	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/runtime"
)

// TestConfigChangeDuringTurnAppliesToNextTurn: an accepted change during
// an in-flight turn never fails and takes effect on the next native
// thread start/resume.
func TestConfigChangeDuringTurnAppliesToNextTurn(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("cfg2", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, "hold", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	if err := e.adapter.ApplyConfig(context.Background(), m,
		ConfigCommand{
			Model: "m2",
			Mode:  "fast",
		}); err != nil {
		t.Fatalf("config during a turn must be accepted: %v", err)
	}
	e.waitDurable(m.Session.ID, api.EventConfigChanged)
	if got := e.reload(m.Session.ID); got.Model != "m2" || got.Mode != "fast" {
		t.Fatalf("accepted config = %q/%q", got.Model, got.Mode)
	}
	// Finish the turn, then restart the runtime: the next generation must
	// reattach the thread with the newly accepted model.
	var req inputRequestedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventInputRequested), &req); err != nil {
		t.Fatal(err)
	}
	if err := e.adapter.AnswerInput(context.Background(), m, req.InputID,
		[]AnswerSelection{{QuestionID: "q1", Answers: []string{"alpha"}}}); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	os.Setenv("FAKE_CODEX_MODE", "happy")
	defer os.Setenv("FAKE_CODEX_MODE", "input")
	if err := e.sup.StopAll(); err != nil {
		t.Fatal(err)
	}
	e.sup = runtime.New()
	e.sup.OnGone = e.adapter.OnRuntimeGone
	e.adapter.deps.Supervisor = e.sup
	if _, err := e.adapter.Prompt(context.Background(), m, "again", "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resume record", func() bool {
		return e.durablePayload(m.Session.ID, api.EventHarnessStarted) != nil
	})
	tr, err := e.store.Transcript(m.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	lastModel := ""
	for _, r := range recs {
		if r.Type != api.EventHarnessStarted {
			continue
		}
		var started harnessStartedPayload
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.Resumed {
			lastModel = started.Model
		}
	}
	if lastModel != "m2" {
		t.Fatalf("resumed thread model = %q, want the accepted m2", lastModel)
	}
}

// TestConfigChangeIsAcceptedAndDurable: model/mode changes are recorded
// and applied on the next turn; a change during a turn is refused.
func TestConfigChangeIsAcceptedAndDurable(t *testing.T) {
	e := newEnv(t, "happy")
	m := e.newSession("cfg", t.TempDir())
	if err := e.adapter.ApplyConfig(context.Background(), m, ConfigCommand{
		Model: "m1",
		Mode:  "fast",
	}); err != nil {
		t.Fatal(err)
	}
	got := e.reload(m.Session.ID)
	if got.Model != "m1" || got.Mode != "fast" {
		t.Fatalf("config = %q/%q", got.Model, got.Mode)
	}
	if p := e.durablePayload(m.Session.ID, api.EventConfigChanged); p == nil {
		t.Fatal("no durable session.config record")
	}
	// The accepted model is what the native thread start receives.
	if _, err := e.adapter.Prompt(context.Background(), m, "hello", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventHarnessStarted)
	var started harnessStartedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventHarnessStarted), &started); err != nil {
		t.Fatal(err)
	}
	if started.Model != "m1" {
		t.Fatalf("thread started with model %q, want m1", started.Model)
	}
}
