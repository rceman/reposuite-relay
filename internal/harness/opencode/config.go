package opencode

import (
	"context"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// ApplyConfig applies an accepted model/mode change.
//
// Model: OpenCode advertises a `model` config option (category "model") whose
// values are the runtime's own model ids, and session/set_config_option mutates
// it mid-session — so a live session is mutated natively and a cold one records
// the DESIRED choice for the next native materialization. The option id and the
// value are DISCOVERED from the protocol; Relay never hard-codes a model list,
// and it records the change only after the runtime accepted it.
//
// Mode: applied through the advertised mode option (category "mode") when the
// runtime advertises one. A runtime that advertises no mode option yields
// UNSUPPORTED_OPERATION rather than a fabricated mapping.
//
// RelaySession.Model/Mode are the durable DESIRED configuration. They are not
// proof that the current native session already uses those values: effective
// values only ever come from what the runtime reported.
func (a *Adapter) ApplyConfig(ctx context.Context, m *session.Managed, cmd harness.ConfigCommand) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the opencode adapter", m.Session.ID)
	}
	desired := desiredConfig(m, cmd)
	snap := m.Snapshot()
	if snap.Model == desired.Model && snap.Mode == desired.Mode {
		return nil
	}
	if err := a.configGuard(m); err != nil {
		return err
	}
	a.mu.Lock()
	handle := st.handle
	a.mu.Unlock()

	// A live handle is mutated natively — a real native mutation, so it is a
	// sleep blocker and serializes against overlapping config/cancel work.
	if handle != nil {
		if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
			return err
		}
		defer a.deps.Supervisor.EndMutation(m.Session.ID)
		if err := a.applyDesired(ctx, handle, desired); err != nil {
			return err
		}
	}
	if err := a.deps.Materialize(m, harness.SessionUpdate{
		Model: &desired.Model,
		Mode:  &desired.Mode,
	}); err != nil {
		return err
	}
	// Effective metrics only from values the runtime actually reported: a
	// COLD deferred change claims no native effect at all.
	if handle != nil {
		a.publishMetrics(m, "config", acp.MergeMetrics(a.Metrics(m.Session.ID), observedConfig(handle)))
	}
	return a.publishDurable(m, api.EventConfigChanged, api.ConfigChangedPayload{
		Model: desired.Model,
		Mode:  desired.Mode,
	})
}

// configGuard rejects a live configuration change while native work is in
// flight: Relay never mutates model/mode underneath an in-flight turn or an
// unresolved input request.
func (a *Adapter) configGuard(m *session.Managed) error {
	switch a.deps.Supervisor.Activity(m.Session.ID) {
	case runtime.ActivityActive, runtime.ActivityWaitingInput:
		return fmt.Errorf("%w: session %s has native work in flight", harness.ErrBusy, m.Session.ID)
	}
	return nil
}

// desiredConfig normalizes a config command against the durable desired
// configuration: an absent (empty) field is left unchanged.
func desiredConfig(m *session.Managed, cmd harness.ConfigCommand) harness.ConfigCommand {
	snap := m.Snapshot()
	out := harness.ConfigCommand{Model: snap.Model, Mode: snap.Mode}
	if cmd.Model != "" {
		out.Model = cmd.Model
	}
	if cmd.Mode != "" {
		out.Mode = cmd.Mode
	}
	return out
}

// applyDesired applies every non-empty desired value through the runtime's
// ADVERTISED config surface. It is used both for a live mutation and for the
// deferred application at first materialization, so a prompt can never run on
// a native session that has not yet been configured as the user asked.
func (a *Adapter) applyDesired(ctx context.Context, handle *acp.Session, desired harness.ConfigCommand) error {
	if desired.Mode != "" {
		if err := applyOption(ctx, handle, acp.CategoryMode, "mode", desired.Mode, "mode"); err != nil {
			return err
		}
	}
	if desired.Model != "" {
		if err := applyOption(ctx, handle, acp.CategoryModel, "model", desired.Model, "model"); err != nil {
			return err
		}
	}
	return nil
}

// applyOption validates a value against the advertised option and applies it
// natively. An already-current value costs no protocol call.
func applyOption(ctx context.Context, handle *acp.Session, category, id, value, what string) error {
	opt, ok := acp.FindOption(handle.Options(), category, id)
	if !ok {
		return fmt.Errorf("%w: runtime advertises no %s config option", harness.ErrUnsupported, what)
	}
	if !optionHasValue(opt, value) {
		return fmt.Errorf("%w: %s %q is not advertised by the runtime", harness.ErrInvalidConfig, what, value)
	}
	if cur, _ := opt.CurrentString(); cur == value {
		return nil
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, value, false); err != nil {
		// The runtime rejected the value: never record it as applied.
		return fmt.Errorf("%w: %v", harness.ErrInvalidConfig, err)
	}
	return nil
}

// observedConfig projects the values the runtime currently reports, so a
// metrics update never claims a value Relay did not observe natively.
func observedConfig(handle *acp.Session) *harness.SessionMetrics {
	out := &harness.SessionMetrics{}
	if opt, ok := acp.FindOption(handle.Options(), acp.CategoryModel, "model"); ok {
		if v, ok := opt.CurrentString(); ok {
			out.Model = v
		}
	}
	if opt, ok := acp.FindOption(handle.Options(), acp.CategoryMode, "mode"); ok {
		if v, ok := opt.CurrentString(); ok {
			out.Mode = v
		}
	}
	return out
}

// optionHasValue reports whether a select option advertises a value.
func optionHasValue(opt acp.ConfigOption, value string) bool {
	for _, v := range opt.SelectOptions() {
		if v.Value == value {
			return true
		}
	}
	return false
}

// compile-time assertion: the adapter satisfies the shared contract.
var _ harness.Adapter = (*Adapter)(nil)
