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

// Prompt accepts a user prompt and submits it as a native turn:
//
//	message.user (durable, accepted by Relay) → runtime ensure/initialize
//	(partitioned by the process model) → session/new or session/load with the
//	EXACT proven slug → session/prompt → in-flight turn.
//
// A failure after acceptance publishes a durable turn.failed record, so an
// accepted prompt is never erased from history.
func (a *Adapter) Prompt(ctx context.Context, m *session.Managed, cmd harness.PromptCommand) (harness.PromptResult, error) {
	st := a.state(m.Session.ID)
	if st == nil {
		return harness.PromptResult{}, fmt.Errorf("session %s not tracked by the devin adapter", m.Session.ID)
	}
	a.mu.Lock()
	if st.turn != nil {
		turnID := st.turnID
		a.mu.Unlock()
		return harness.PromptResult{}, fmt.Errorf("%w: turn %s in flight", harness.ErrBusy, turnID)
	}
	a.mu.Unlock()

	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)

	model := cmd.Model
	if model == "" {
		model = m.Snapshot().Model
	}
	if err := a.publishDurable(m, api.EventMessageUser, api.MessageUserPayload{
		Text:   cmd.Text,
		Model:  model,
		Effort: cmd.Effort,
	}); err != nil {
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		return harness.PromptResult{}, err
	}

	fail := func(err error) (harness.PromptResult, error) {
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{Error: err.Error()})
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
		return harness.PromptResult{}, err
	}

	handle, runtimeKey, err := a.attach(ctx, m, st)
	if err != nil {
		return fail(err)
	}
	// Re-assert the blocker AFTER attach: activity is only recorded for a
	// BOUND session, so the wake path (which binds) must re-assert it or the
	// first turn of a COLD session would not block runtime sleep.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	turn, err := handle.StartPrompt(cmd.Text)
	if err != nil {
		return fail(err)
	}
	a.mu.Lock()
	st.turn = turn
	st.turnID = turn.ID
	a.mu.Unlock()
	_ = a.setSessionState(m, session.StateActive)

	go a.completeTurn(m, handle, turn)
	return harness.PromptResult{
		TurnID:          turn.ID,
		NativeSessionID: handle.ID(),
		RuntimeID:       runtimeKey,
	}, nil
}

// attach returns a usable session handle for a prompt. Devin resumes ONLY a
// proven slug: an unproven or absent identity creates a new native session,
// because a zero-turn Devin session cannot be reattached.
func (a *Adapter) attach(ctx context.Context, m *session.Managed, st *sessState) (*acp.Session, string, error) {
	a.mu.Lock()
	handle, srv, nativeID, proven := st.handle, st.srv, st.nativeID, st.materialized
	a.mu.Unlock()
	if handle != nil && srv != nil {
		return handle, RuntimeKeyFor(m.Snapshot().Model), nil
	}
	// A malformed durable identity is rejected BEFORE any runtime is spawned: a
	// non-empty slug Relay cannot parse is durable corruption, and it is never
	// silently replaced by a fresh native session.
	if nativeID != "" && !harness.ValidNativeID(nativeID) {
		return nil, "", fmt.Errorf("%w: malformed native session id %q", harness.ErrNativeSessionLost, nativeID)
	}

	cwd := m.Snapshot().Cwd
	model := m.Snapshot().Model
	srv, runtimeKey, err := a.ensureRuntime(ctx, cwd, model)
	if err != nil {
		return nil, "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	if !proven {
		// No proven identity: create. (An unproven in-memory slug is dropped
		// with its dead generation — it was never resumable.)
		newHandle, err := acp.OpenSession(callCtx, srv, cwd)
		if err != nil {
			return nil, "", fmt.Errorf("%w: session/new: %v", harness.ErrRuntimeUnavailable, err)
		}
		if err := a.applyPermissionMode(callCtx, m, newHandle); err != nil {
			newHandle.Close()
			return nil, "", err
		}
		if err := a.bind(m.Session.ID, runtimeKey, srv, newHandle); err != nil {
			return nil, "", err
		}
		if err := a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
			RuntimeID: runtimeKey,
			Model:     model,
			Resumed:   false,
		}); err != nil {
			return nil, "", err
		}
		return newHandle, runtimeKey, nil
	}

	loaded, err := acp.AttachSession(callCtx, srv, cwd, nativeID)
	if err != nil {
		// Fail closed: never substitute a fresh session for the exact one.
		return nil, "", fmt.Errorf("%w: session/load %s: %v", harness.ErrNativeSessionLost, nativeID, err)
	}
	if err := a.applyPermissionMode(callCtx, m, loaded); err != nil {
		loaded.Close()
		return nil, "", err
	}
	if err := a.bind(m.Session.ID, runtimeKey, srv, loaded); err != nil {
		return nil, "", err
	}
	// A new generation took ownership of the proven identity: +1.
	if err := a.deps.Materialize(m, harness.SessionUpdate{BumpGeneration: true}); err != nil {
		return nil, "", err
	}
	if err := a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
		RuntimeID:       runtimeKey,
		NativeSessionID: nativeID,
		Model:           model,
		Resumed:         true,
	}); err != nil {
		return nil, "", err
	}
	return loaded, runtimeKey, nil
}

// applyPermissionMode selects the advertised Devin mode that grants Relay's
// configured permissions. The mode id is DISCOVERED from the protocol: when the
// runtime does not advertise the bypass mode, Relay leaves the runtime's own
// default in place rather than guessing at an equivalent.
func (a *Adapter) applyPermissionMode(ctx context.Context, m *session.Managed, handle *acp.Session) error {
	want := m.Snapshot().Mode
	if want == "" {
		want = BypassMode
	}
	opt, ok := acp.FindOption(handle.Options(), acp.CategoryMode, "mode")
	if !ok {
		if m.Snapshot().Mode != "" {
			return fmt.Errorf("%w: runtime advertises no mode config option", harness.ErrUnsupported)
		}
		return nil
	}
	if !optionHasValue(opt, want) {
		if m.Snapshot().Mode != "" {
			return fmt.Errorf("%w: mode %q is not advertised by the runtime", harness.ErrInvalidConfig, want)
		}
		// No bypass mode on offer: the runtime's own default stands, and Relay
		// records no mode it did not set.
		return nil
	}
	if handle.CurrentMode() == want {
		return a.recordMode(m, want)
	}
	if _, err := handle.SetConfigValue(ctx, opt.ID, want, false); err != nil {
		return fmt.Errorf("%w: mode %q: %v", harness.ErrInvalidConfig, want, err)
	}
	return a.recordMode(m, want)
}

// recordMode persists a mode the runtime accepted.
func (a *Adapter) recordMode(m *session.Managed, mode string) error {
	if m.Snapshot().Mode == mode {
		return nil
	}
	return a.deps.Materialize(m, harness.SessionUpdate{Mode: &mode})
}

// completeTurn awaits the terminal turn result and publishes the canonical
// terminal record. A COMPLETED first turn proves the native slug is resumable,
// and only then is it persisted — the Devin materialization rule.
func (a *Adapter) completeTurn(m *session.Managed, handle *acp.Session, turn *acp.Turn) {
	stopReason, text, _, usage, err := turn.Result()
	turnID := turn.ID

	// A completed turn proves the slug is resumable. This runs BEFORE the
	// terminal record and outside the adapter lock (persistNative takes it).
	if err == nil && stopReason == acp.StopEndTurn {
		if persistErr := a.persistNative(m, handle.ID()); persistErr != nil {
			err = persistErr
		}
	}
	a.mu.Lock()
	switch {
	case err != nil:
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: turnID,
			Error:  "turn ended: " + err.Error(),
		})
	case stopReason == acp.StopEndTurn:
		_ = a.publishDurable(m, api.EventMessageAgentCompleted, api.MessageCompletedPayload{
			TurnID: turnID,
			Text:   text,
		})
	case stopReason == acp.StopCancelled:
		_ = a.publishDurable(m, api.EventTurnInterrupted, api.TurnEventPayload{TurnID: turnID})
	case stopReason == "":
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: turnID,
			Error:  "turn ended without a stop reason",
		})
	default:
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: turnID,
			Error:  "turn stopped: " + stopReason,
		})
	}
	if st := a.sessions[m.Session.ID]; st != nil && st.turn == turn {
		st.turn = nil
		st.turnID = ""
	}
	a.mu.Unlock()

	if metrics := acp.MergeMetrics(a.Metrics(m.Session.ID), acp.UsageMetrics(usage)); metrics != nil {
		a.publishMetrics(m, "turn", metrics)
	}
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
	_ = a.setSessionState(m, session.StateIdle)
}

// persistNative durably records a slug that a completed turn proved resumable.
// Until this runs the slug is in-memory only: a zero-turn Devin session cannot
// be reattached, so persisting it early would create an identity Relay could
// not honor.
func (a *Adapter) persistNative(m *session.Managed, nativeID string) error {
	if !harness.ValidNativeID(nativeID) {
		return fmt.Errorf("%w: unusable native session id %q", harness.ErrNativeSessionLost, nativeID)
	}
	a.mu.Lock()
	st := a.sessions[m.Session.ID]
	first := st != nil && !st.materialized
	a.mu.Unlock()
	if !first {
		return nil
	}
	if err := a.deps.Materialize(m, harness.SessionUpdate{
		NativeSessionID: &nativeID,
		BumpGeneration:  true,
	}); err != nil {
		return err
	}
	a.mu.Lock()
	if st := a.sessions[m.Session.ID]; st != nil {
		st.nativeID = nativeID
		st.materialized = true
		a.byNative[nativeID] = m.Session.ID
	}
	a.mu.Unlock()
	return a.publishDurable(m, api.EventNativeSession, api.NativeSessionPayload{
		NativeSessionID: nativeID,
		Generation:      m.Snapshot().Generation,
	})
}

// Cancel interrupts the in-flight native turn through session/cancel. The
// terminal turn.interrupted record comes from the pending prompt response.
func (a *Adapter) Cancel(_ context.Context, m *session.Managed) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the devin adapter", m.Session.ID)
	}
	a.mu.Lock()
	handle, turn := st.handle, st.turn
	a.mu.Unlock()
	if handle == nil || turn == nil {
		return fmt.Errorf("%w: session %s has no in-flight turn", harness.ErrNoActiveTurn, m.Session.ID)
	}
	if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
		return err
	}
	defer a.deps.Supervisor.EndMutation(m.Session.ID)
	if err := handle.Cancel(); err != nil {
		return fmt.Errorf("session/cancel: %w", err)
	}
	return nil
}

// AnswerInput is not implemented for Devin: the runtime exposes approvals
// (handled by policy), not Relay's requested-input surface, and an approval must
// never be silently converted into user input.
func (a *Adapter) AnswerInput(_ context.Context, m *session.Managed, _ string, _ []harness.InputAnswer) error {
	return fmt.Errorf("%w: devin has no requested-input surface; approvals are handled by policy",
		harness.ErrUnsupported)
}
