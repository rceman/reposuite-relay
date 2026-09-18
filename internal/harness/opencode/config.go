package opencode

import (
	"context"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// ApplyConfig applies an accepted model/mode change.
//
// Model: OpenCode advertises a `model` config option (category "model") whose
// values are the runtime's own model ids, and session/set_config_option mutates
// it mid-session — so a live session is mutated natively and a cold one records
// the choice for the next native materialization. The option id and the value
// are DISCOVERED from the protocol; Relay never hard-codes a model list, and it
// records the change only after the runtime accepted it.
//
// Mode: applied through the advertised mode option (category "mode") when the
// runtime advertises one. A runtime that advertises no mode option yields
// UNSUPPORTED_OPERATION rather than a fabricated mapping.
func (a *Adapter) ApplyConfig(ctx context.Context, m *session.Managed, cmd harness.ConfigCommand) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the opencode adapter", m.Session.ID)
	}
	snap := m.Snapshot()
	if snap.Model == cmd.Model && snap.Mode == cmd.Mode {
		return nil
	}
	if cmd.Mode != "" && cmd.Mode != snap.Mode {
		if err := a.applyMode(ctx, st, cmd.Mode); err != nil {
			return err
		}
	}
	if cmd.Model != "" && cmd.Model != snap.Model {
		if err := a.applyModel(ctx, st, cmd.Model); err != nil {
			return err
		}
	}
	if err := a.deps.Materialize(m, harness.SessionUpdate{Model: &cmd.Model, Mode: &cmd.Mode}); err != nil {
		return err
	}
	metrics := mergeMetrics(a.Metrics(m.Session.ID), &harness.SessionMetrics{
		Model: cmd.Model,
		Mode:  cmd.Mode,
	})
	a.publishMetrics(m, "config", metrics)
	return a.publishDurable(m, api.EventConfigChanged, api.ConfigChangedPayload{
		Model: cmd.Model,
		Mode:  cmd.Mode,
	})
}

// applyMode applies a mode through the runtime's advertised mode option. A
// cold session has nothing native to mutate yet: the choice is recorded and
// applied at the next materialization.
func (a *Adapter) applyMode(ctx context.Context, st *sessState, mode string) error {
	a.mu.Lock()
	handle := st.handle
	a.mu.Unlock()
	if handle == nil {
		return nil
	}
	opt, ok := acp.FindOption(handle.Options(), acp.CategoryMode, "mode")
	if !ok {
		return fmt.Errorf("%w: runtime advertises no mode config option", harness.ErrUnsupported)
	}
	if !optionHasValue(opt, mode) {
		return fmt.Errorf("%w: mode %q is not advertised", harness.ErrInvalidConfig, mode)
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, mode, false); err != nil {
		return fmt.Errorf("%w: %v", harness.ErrInvalidConfig, err)
	}
	return nil
}

// applyModel applies a model through the advertised model option. On a cold
// session the value is only recorded: Relay does not claim a native effect it
// could not observe.
func (a *Adapter) applyModel(ctx context.Context, st *sessState, model string) error {
	a.mu.Lock()
	handle := st.handle
	a.mu.Unlock()
	if handle == nil {
		return nil
	}
	opt, ok := acp.FindOption(handle.Options(), acp.CategoryModel, "model")
	if !ok {
		return fmt.Errorf("%w: runtime advertises no model config option", harness.ErrUnsupported)
	}
	if !optionHasValue(opt, model) {
		return fmt.Errorf("%w: model %q is not advertised by the runtime", harness.ErrInvalidConfig, model)
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, model, false); err != nil {
		// The runtime rejected the value: never record it as applied.
		return fmt.Errorf("%w: %v", harness.ErrInvalidConfig, err)
	}
	return nil
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
