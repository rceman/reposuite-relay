package devin

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
// Mode: applied through the runtime's advertised mode option and recorded only
// after the runtime accepted it.
//
// Model: Devin chooses its model when the PROCESS starts, so a live runtime
// cannot switch models for a running session. Relay records the choice durably
// and, when the live generation's process model no longer matches the desired
// one, DETACHES the session from that generation: the next prompt starts (or
// reuses) a generation partitioned by the new model. The old shared runtime is
// left running for its other sessions. When the runtime does advertise a live
// model option, that option is used instead and no migration happens.
func (a *Adapter) ApplyConfig(ctx context.Context, m *session.Managed, cmd harness.ConfigCommand) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the devin adapter", m.Session.ID)
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
	handle, liveKey := st.handle, st.liveRuntimeKey
	a.mu.Unlock()

	detached := false
	if handle != nil {
		if desired.Mode != snap.Mode {
			err := a.liveMutation(m, func() error {
				return a.applyMode(ctx, handle, desired.Mode)
			})
			if err != nil {
				return err
			}
		}
		if desired.Model != snap.Model {
			if opt, ok := acp.FindOption(handle.Options(), acp.CategoryModel, "model"); ok {
				err := a.liveMutation(m, func() error {
					return a.applyModelOption(ctx, handle, opt, desired.Model)
				})
				if err != nil {
					return err
				}
			} else if liveKey != RuntimeKeyFor(desired.Model) {
				// Process-level model: the live generation belongs to a
				// different process model and must never serve this session.
				a.detach(m.Session.ID)
				detached = true
			}
		}
	}
	if err := a.deps.Materialize(m, harness.SessionUpdate{
		Model: &desired.Model,
		Mode:  &desired.Mode,
	}); err != nil {
		return err
	}
	// Effective metrics come only from values the runtime actually reported.
	if handle != nil && !detached {
		a.publishMetrics(m, "config", acp.MergeMetrics(a.Metrics(m.Session.ID), observedConfig(handle)))
	}
	return a.publishDurable(m, api.EventConfigChanged, api.ConfigChangedPayload{
		Model: desired.Model,
		Mode:  desired.Mode,
	})
}

// configGuard rejects a live configuration change while native work is in
// flight: Relay never mutates model/mode underneath an in-flight turn, and it
// never migrates a session between process generations mid-turn.
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

// liveMutation runs one native mutation as a supervisor sleep blocker.
func (a *Adapter) liveMutation(m *session.Managed, fn func() error) error {
	if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
		return err
	}
	defer a.deps.Supervisor.EndMutation(m.Session.ID)
	return fn()
}

// applyMode applies a mode through the advertised mode option. A COLD session
// has nothing native to mutate: the choice is recorded and applied at the next
// materialization.
func (a *Adapter) applyMode(ctx context.Context, handle *acp.Session, mode string) error {
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

// applyModelOption applies a model through an advertised model option (a
// runtime that offers live model mutation needs no process migration).
func (a *Adapter) applyModelOption(ctx context.Context, handle *acp.Session, opt acp.ConfigOption, model string) error {
	if !optionHasValue(opt, model) {
		return fmt.Errorf("%w: model %q is not advertised by the runtime", harness.ErrInvalidConfig, model)
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, model, false); err != nil {
		return fmt.Errorf("%w: %v", harness.ErrInvalidConfig, err)
	}
	return nil
}

// observedConfig projects the values the runtime currently reports.
func observedConfig(handle *acp.Session) *harness.SessionMetrics {
	out := &harness.SessionMetrics{}
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
