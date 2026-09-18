package opencode

import (
	"encoding/json"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestConfigModelMutatesLiveSession: the model is applied through the
// runtime's own advertised config option, and only then recorded.
func TestConfigModelMutatesLiveSession(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "model", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return !a.State(m.Session.ID).TurnInFlight })

	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{
		Model: "fake/model-b",
		Mode:  m.Snapshot().Mode,
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.Reload(m.Session.ID).Model; got != "fake/model-b" {
		t.Fatalf("model = %q, want the accepted value", got)
	}
	var cfg api.ConfigChangedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventConfigChanged), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "fake/model-b" {
		t.Fatalf("session.config payload = %+v", cfg)
	}
	if n := acp.CountFakeEvents(e.ACPState, "set_config_option"); n != 1 {
		t.Fatalf("native set_config_option calls = %d, want 1", n)
	}
	if m := a.Metrics(m.Session.ID); m == nil || m.Model != "fake/model-b" {
		t.Fatalf("metrics = %+v", m)
	}
}

// TestConfigRejectsUnadvertisedModel: a value the runtime does not advertise is
// rejected and NEVER recorded as applied.
func TestConfigRejectsUnadvertisedModel(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "bad-model", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return !a.State(m.Session.ID).TurnInFlight })

	err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "fake/model-zzz"})
	if !isInvalidConfig(err) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if got := e.Reload(m.Session.ID).Model; got == "fake/model-zzz" {
		t.Fatal("a rejected model must not be recorded")
	}
	if e.DurablePayload(m.Session.ID, api.EventConfigChanged) != nil {
		t.Fatal("a rejected model must not publish session.config")
	}
	if n := acp.CountFakeEvents(e.ACPState, "set_config_option"); n != 0 {
		t.Fatalf("no native mutation should be attempted for an unadvertised value (%d)", n)
	}
}

// TestConfigModeUsesAdvertisedOption: the mode is applied through the
// advertised mode config option, discovered from the protocol.
func TestConfigModeUsesAdvertisedOption(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "mode", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return !a.State(m.Session.ID).TurnInFlight })

	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Mode: "ask"}); err != nil {
		t.Fatal(err)
	}
	if got := e.Reload(m.Session.ID).Mode; got != "ask" {
		t.Fatalf("mode = %q", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "set_config_option"); n != 1 {
		t.Fatalf("native set_config_option calls = %d, want 1", n)
	}
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Mode: "nonsense-mode"}); !isInvalidConfig(err) {
		t.Fatalf("err = %v, want ErrInvalidConfig for an unadvertised mode", err)
	}
}

// TestNoOpConfigIsNotRecorded: an unchanged configuration is a no-op, so no
// spurious event is published.
func TestNoOpConfigIsNotRecorded(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "noop", t.TempDir())
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{}); err != nil {
		t.Fatal(err)
	}
	if e.DurablePayload(m.Session.ID, api.EventConfigChanged) != nil {
		t.Fatal("an unchanged configuration must not publish session.config")
	}
	if n := acp.CountFakeEvents(e.ACPState, "set_config_option"); n != 0 {
		t.Fatalf("native mutations = %d, want 0", n)
	}
}

// TestAnswerInputIsUnsupported: OpenCode has no requested-input surface, and an
// approval must never be converted into user input.
func TestAnswerInputIsUnsupported(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "input", t.TempDir())
	if err := a.AnswerInput(bg(), m, "in-1", []harness.InputAnswer{
		{QuestionID: "q1", Answers: []string{"yes"}},
	}); !isUnsupported(err) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestTrackRestoresColdIdentity: a daemon restart restores the exact durable
// identity COLD, without any runtime.
func TestTrackRestoresColdIdentity(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "restore", t.TempDir())
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "ses_424242"
	m.Session.Generation = 3
	m.MetaMu.Unlock()

	fresh := NewAdapter(Deps{
		Broker:        e.Broker,
		Supervisor:    e.Supervisor,
		Command:       harnessenv.FakeACPCommand(t, acp.FakeVendorOpenCode, acp.FakeHappy, e.ACPState),
		Materialize:   e.Materialize,
		RandRuntimeID: func() (string, error) { return "rt-2", nil },
		Version:       "test",
	})
	fresh.Track(m)
	st := fresh.State(m.Session.ID)
	if !st.Materialized || st.NativeSessionID != "ses_424242" || st.Live {
		t.Fatalf("restored state = %+v", st)
	}
	if fresh.Name() != session.HarnessOpenCode {
		t.Fatalf("name = %q", fresh.Name())
	}
	if fresh.Metrics(m.Session.ID) != nil {
		t.Fatal("a restored COLD session has no metrics")
	}
}
