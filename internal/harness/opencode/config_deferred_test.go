package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
)

// TestConfigOnColdSessionDoesNotWakeRuntime: a COLD session has nothing native
// to mutate, so the DESIRED choice is recorded without waking a process, and
// it is applied natively — through the advertised option, BEFORE the prompt —
// at the next materialization.
func TestConfigOnColdSessionDoesNotWakeRuntime(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "cold-config", t.TempDir())
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "fake/model-b"}); err != nil {
		t.Fatal(err)
	}
	if got := e.Reload(m.Session.ID).Model; got != "fake/model-b" {
		t.Fatalf("model = %q", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 0 {
		t.Fatalf("a cold config change must not wake a runtime (initialize = %d)", n)
	}
	if e.Supervisor.Len() != 0 {
		t.Fatalf("runtime count = %d, want 0", e.Supervisor.Len())
	}
	// The deferred change claims NO effective state before materialization.
	if mets := a.Metrics(m.Session.ID); mets != nil && mets.Model != "" {
		t.Fatalf("deferred config must not be reported effective: %+v", mets)
	}
	// First prompt: session/new, then the advertised model option is applied,
	// then session/prompt — in exactly that order.
	if _, err := a.Prompt(bg(), m, text("hello")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	ev := acp.ReadFakeEvents(e.ACPState)
	var idxNew, idxCfg, idxPrompt = -1, -1, -1
	for i, line := range ev {
		switch {
		case strings.HasPrefix(line, "session/new"):
			idxNew = i
		case strings.HasPrefix(line, "set_config_option\tmodel=fake/model-b"):
			idxCfg = i
		case strings.HasPrefix(line, "session/prompt"):
			idxPrompt = i
		}
	}
	if idxNew < 0 || idxCfg < 0 || idxPrompt < 0 {
		t.Fatalf("missing native observations: %v", ev)
	}
	if !(idxNew < idxCfg && idxCfg < idxPrompt) {
		t.Fatalf("desired model was not applied before the prompt: %v", ev)
	}
	if got := e.Reload(m.Session.ID).Model; got != "fake/model-b" {
		t.Fatalf("model after wake = %q", got)
	}
	// The native fake reports the applied model as current.
	if mets := a.Metrics(m.Session.ID); mets == nil || mets.Model != "fake/model-b" {
		t.Fatalf("effective model = %+v, want the observed fake/model-b", mets)
	}
}

// TestDeferredInvalidConfigFailsBeforePrompt: a deferred desired value that
// the runtime cannot apply is rejected at first materialization — no user turn
// is ever submitted and no effective value is claimed.
func TestDeferredInvalidConfigFailsBeforePrompt(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "bad-deferred", t.TempDir())
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "fake/model-zzz"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(bg(), m, text("hello")); !isInvalidConfig(err) {
		t.Fatalf("err = %v, want ErrInvalidConfig before any prompt", err)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/prompt"); n != 0 {
		t.Fatalf("session/prompt calls = %d, want 0", n)
	}
	if e.DurablePayload(m.Session.ID, api.EventMessageAgentCompleted) != nil {
		t.Fatal("a rejected desired config must not produce a completed turn")
	}
	if mets := a.Metrics(m.Session.ID); mets != nil && mets.Model == "fake/model-zzz" {
		t.Fatal("a rejected value must never be reported effective")
	}
}

// TestPromptOverridesAreRejected: per-turn model/effort overrides have no
// verified OpenCode equivalent, so they are rejected BEFORE the prompt is
// accepted — no durable message.user, no native prompt.
func TestPromptOverridesAreRejected(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "override", t.TempDir())
	for _, cmd := range []harness.PromptCommand{
		{Text: "hi", Model: "fake/model-b"},
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
// flight is rejected with SESSION_BUSY — no mid-turn mutation.
func TestConfigDuringActiveTurnIsBusy(t *testing.T) {
	e, a := newEnv(t, acp.FakeCancel)
	m := newSession(t, e, a, "busy-cfg", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hold")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "turn in flight", func() bool { return a.State(m.Session.ID).TurnInFlight })
	if err := a.ApplyConfig(bg(), m, harness.ConfigCommand{Model: "fake/model-b"}); !isBusy(err) {
		t.Fatalf("err = %v, want ErrBusy during an in-flight turn", err)
	}
	if err := a.Cancel(bg(), m); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventTurnInterrupted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if got := e.Reload(m.Session.ID).Model; got == "fake/model-b" {
		t.Fatal("a rejected config change must not be recorded")
	}
}

// TestLiveConfigMutationBlocksRuntimeSleep: while the native config RPC is in
// flight, the runtime must not sleep.
func TestLiveConfigMutationBlocksRuntimeSleep(t *testing.T) {
	e, a := newEnv(t, acp.FakeConfigSlow)
	m := newSession(t, e, a, "slow-cfg", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("one")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })

	done := make(chan error, 1)
	go func() {
		done <- a.ApplyConfig(bg(), m, harness.ConfigCommand{
			Model: "fake/model-b", Mode: m.Snapshot().Mode,
		})
	}()
	harnessenv.WaitFor(t, "native mutation blocked", func() bool {
		return acp.CountFakeEvents(e.ACPState, "config-blocked") > 0
	})
	if err := e.Supervisor.StopIfIdle(RuntimeKey); !errors.Is(err, runtime.ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle = %v, want ErrRuntimeBusy while config is in flight", err)
	}
	// Release the native mutation.
	if err := os.WriteFile(filepath.Join(e.ACPState, "release-config"), []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := e.Supervisor.StopIfIdle(RuntimeKey); err != nil {
		t.Fatalf("StopIfIdle after config = %v, want success", err)
	}
}
