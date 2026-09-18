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
	// A per-turn model/effort override has no verified Devin ACP equivalent,
	// so Relay rejects it deterministically BEFORE the prompt is accepted: it
	// is never silently ignored and never reinterpreted as a persistent
	// config change.
	if cmd.Model != "" || cmd.Effort != "" {
		return harness.PromptResult{}, fmt.Errorf(
			"%w: devin has no per-turn model/effort override; use session config",
			harness.ErrUnsupported)
	}
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

	model := m.Snapshot().Model
	if err := a.publishDurable(m, api.EventMessageUser, api.MessageUserPayload{
		Text:  cmd.Text,
		Model: model,
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

	handle, err := a.attach(ctx, m, st)
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
		RuntimeID:       a.runtimeID(m.Session.ID),
	}, nil
}

// runtimeID returns the actual ephemeral identity of the runtime generation
// currently bound to the session — never the supervisor's stable key.
func (a *Adapter) runtimeID(sessionID string) string {
	v, ok := a.deps.Supervisor.View(sessionID)
	if !ok {
		return ""
	}
	return v.RuntimeID
}

// attach returns a usable session handle for a prompt. A live handle is reused
// ONLY when its generation's process model still matches the desired one:
// Devin's model is process-level, so a handle partitioned for another model
// must never serve this session.
//
// Devin resumes ONLY a proven slug: an unproven or absent identity creates a
// new native session, because a zero-turn Devin session cannot be reattached.
func (a *Adapter) attach(ctx context.Context, m *session.Managed, st *sessState) (*acp.Session, error) {
	model := m.Snapshot().Model
	a.mu.Lock()
	handle, srv, nativeID := st.handle, st.srv, st.nativeID
	proven, liveKey := st.materialized, st.liveRuntimeKey
	a.mu.Unlock()
	if handle != nil && srv != nil && liveKey == RuntimeKeyFor(model) {
		return handle, nil
	}
	// A malformed durable identity is rejected BEFORE any runtime is spawned: a
	// non-empty slug Relay cannot parse is durable corruption, and it is never
	// silently replaced by a fresh native session.
	if nativeID != "" && !harness.ValidNativeID(nativeID) {
		return nil, fmt.Errorf("%w: malformed native session id %q", harness.ErrNativeSessionLost, nativeID)
	}

	cwd := m.Snapshot().Cwd
	srv, runtimeKey, err := a.ensureRuntime(ctx, cwd, model)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	if !proven {
		// No proven identity: create. (An unproven in-memory slug is dropped
		// with its dead generation — it was never resumable.)
		newHandle, err := acp.OpenSession(callCtx, srv, cwd)
		if err != nil {
			return nil, fmt.Errorf("%w: session/new: %v", harness.ErrRuntimeUnavailable, err)
		}
		if err := a.applyPermissionMode(callCtx, m, newHandle); err != nil {
			newHandle.Close()
			return nil, err
		}
		return a.finishAttach(m, runtimeKey, srv, newHandle, "", false)
	}

	loaded, err := acp.AttachSession(callCtx, srv, cwd, nativeID)
	if err != nil {
		// Fail closed: never substitute a fresh session for the exact one.
		return nil, fmt.Errorf("%w: session/load %s: %v", harness.ErrNativeSessionLost, nativeID, err)
	}
	if err := a.applyPermissionMode(callCtx, m, loaded); err != nil {
		loaded.Close()
		return nil, err
	}
	return a.finishAttach(m, runtimeKey, srv, loaded, nativeID, true)
}

// finishAttach completes a bind transaction: bind the session to the
// generation, then record that a successful native BINDING happened. The
// generation counts bindings, not materializations — a failed first turn may
// legitimately leave Generation=1 with no resumable identity.
func (a *Adapter) finishAttach(m *session.Managed, runtimeKey string, srv *acp.Server,
	handle *acp.Session, nativeID string, resumed bool) (*acp.Session, error) {
	if err := a.bind(m.Session.ID, runtimeKey, srv, handle); err != nil {
		return nil, err
	}
	if err := a.deps.Materialize(m, harness.SessionUpdate{BumpGeneration: true}); err != nil {
		a.detach(m.Session.ID)
		return nil, err
	}
	if err := a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
		RuntimeID:       a.runtimeID(m.Session.ID),
		NativeSessionID: nativeID,
		Model:           m.Snapshot().Model,
		Resumed:         resumed,
	}); err != nil {
		return nil, err
	}
	return handle, nil
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
