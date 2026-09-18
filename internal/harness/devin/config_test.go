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

// liveFirstTurn runs one completed turn so the session is live and materialized.
func liveFirstTurn(t *testing.T, e *harnessenv.Env, a *Adapter, m *session.Managed) {
	t.Helper()
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return !a.State(m.Session.ID).TurnInFlight })
}

// TestConfigModeAppliesNatively: an advertised mode is applied through the
// native config surface and recorded only after the runtime accepted it.
func TestConfigModeAppliesNatively(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "mode", t.TempDir())
	liveFirstTurn(t, e, a, m)
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Mode: "ask"}); err != nil {
		t.Fatal(err)
	}
	if got := e.Reload(m.Session.ID).Mode; got != "ask" {
		t.Fatalf("mode = %q", got)
	}
	joined := ""
	for _, line := range acp.ReadFakeEvents(e.ACPState) {
		joined += line + "\n"
	}
	if !contains(joined, "mode=ask") {
		t.Fatalf("events = %v", acp.ReadFakeEvents(e.ACPState))
	}
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Mode: "nonsense"}); !isInvalidConfig(err) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if got := e.Reload(m.Session.ID).Mode; got != "ask" {
		t.Fatalf("mode after rejection = %q, want the accepted value", got)
	}
}

// TestNoOpConfigIsNotRecorded.
func TestNoOpConfigIsNotRecorded(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "noop", t.TempDir())
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{}); err != nil {
		t.Fatal(err)
	}
	if e.DurablePayload(m.Session.ID, api.EventConfigChanged) != nil {
		t.Fatal("an unchanged configuration must not publish session.config")
	}
}

// TestAnswerInputIsUnsupported.
func TestAnswerInputIsUnsupported(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "input", t.TempDir())
	if err := a.AnswerInput(bg(), m, "in-1", []harness.InputAnswer{
		{QuestionID: "q1", Answers: []string{"yes"}},
	}); !isUnsupported(err) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestPermissionIsAutoApprovedAndNeverBecomesInput.
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

// TestUnclassifiablePermissionFailsClosed.
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

// TestMetricsProjection: canonical metrics come from the runtime's own usage
// reporting, and hidden reasoning is never surfaced as assistant text.
func TestMetricsProjection(t *testing.T) {
	e, a := newEnv(t, acp.FakeThought)
	m := newSession(t, e, a, "metrics", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hello")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	var completed api.MessageCompletedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventMessageAgentCompleted), &completed); err != nil {
		t.Fatal(err)
	}
	if contains(completed.Text, "hidden reasoning") {
		t.Fatalf("assistant text leaked reasoning: %q", completed.Text)
	}
	metrics := a.Metrics(m.Session.ID)
	if metrics == nil || metrics.InputTokens == nil || *metrics.InputTokens != 10 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics.ReasoningTokens == nil || *metrics.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %+v", metrics.ReasoningTokens)
	}
	if metrics.ContextLimit == nil || *metrics.ContextLimit != 200000 {
		t.Fatalf("context limit = %+v", metrics.ContextLimit)
	}
}

// TestTrackRestoresColdIdentity: a daemon restart restores the proven slug COLD.
func TestTrackRestoresColdIdentity(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "restore", t.TempDir())
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "calm-river"
	m.Session.Generation = 4
	m.MetaMu.Unlock()

	fresh := NewAdapter(Deps{
		Broker:     e.Broker,
		Supervisor: e.Supervisor,
		Command: func(model string) (acp.Command, error) {
			return harnessenv.FakeACPCommand(t, acp.FakeVendorDevin, acp.FakeHappy, e.ACPState)()
		},
		Materialize:   e.Materialize,
		RandRuntimeID: func() (string, error) { return "rt-2", nil },
		Version:       "test",
	})
	fresh.Track(m)
	st := fresh.State(m.Session.ID)
	if !st.Materialized || st.NativeSessionID != "calm-river" || st.Live {
		t.Fatalf("restored state = %+v", st)
	}
	if fresh.Name() != session.HarnessDevin {
		t.Fatalf("name = %q", fresh.Name())
	}
	if fresh.Metrics(m.Session.ID) != nil {
		t.Fatal("a restored COLD session has no metrics")
	}
}
