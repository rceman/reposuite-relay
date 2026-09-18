package devin

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
// Mode: applied through the runtime's advertised mode option and recorded only
// after the runtime accepted it.
//
// Model: Devin chooses its model when the PROCESS starts, so a live runtime
// cannot switch models for a running session. Relay records the choice durably
// — the next runtime generation is partitioned by that model (RuntimeKeyFor) —
// and does not claim a native mutation it cannot perform. When the runtime does
// advertise a model config option, it is used instead.
func (a *Adapter) ApplyConfig(ctx context.Context, m *session.Managed, cmd harness.ConfigCommand) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the devin adapter", m.Session.ID)
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
	metrics := acp.MergeMetrics(a.Metrics(m.Session.ID), &harness.SessionMetrics{
		Model: cmd.Model,
		Mode:  cmd.Mode,
	})
	a.publishMetrics(m, "config", metrics)
	return a.publishDurable(m, api.EventConfigChanged, api.ConfigChangedPayload{
		Model: cmd.Model,
		Mode:  cmd.Mode,
	})
}

// applyMode applies a mode through the advertised mode option. A COLD session
// has nothing native to mutate: the choice is recorded and applied at the next
// materialization.
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

// applyModel applies a model through an advertised model option when the
// runtime offers one. Otherwise the durable preference stands for the next
// runtime generation, which is a real effect: the generation is keyed by model.
func (a *Adapter) applyModel(ctx context.Context, st *sessState, model string) error {
	a.mu.Lock()
	handle := st.handle
	a.mu.Unlock()
	if handle == nil {
		return nil
	}
	opt, ok := acp.FindOption(handle.Options(), acp.CategoryModel, "model")
	if !ok {
		// Process-level model: no live mutation is possible or claimed.
		return nil
	}
	if !optionHasValue(opt, model) {
		return fmt.Errorf("%w: model %q is not advertised by the runtime", harness.ErrInvalidConfig, model)
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, model, false); err != nil {
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
