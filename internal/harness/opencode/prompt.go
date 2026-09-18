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

// Prompt accepts a user prompt and submits it as a native turn:
//
//	message.user (durable, accepted by Relay) → runtime ensure/initialize
//	→ session/new (first prompt) or session/load (exact resume)
//	→ session/prompt → in-flight turn.
//
// The ACP prompt RESPONSE is the terminal turn result, so the turn is awaited
// asynchronously and completes through completeTurn. A failure after
// acceptance publishes a durable turn.failed record, so an accepted prompt is
// never erased from history.
func (a *Adapter) Prompt(ctx context.Context, m *session.Managed, cmd harness.PromptCommand) (harness.PromptResult, error) {
	// A per-turn model/effort override has no verified OpenCode ACP
	// equivalent, so Relay rejects it deterministically BEFORE the prompt is
	// accepted: it is never silently ignored and never reinterpreted as a
	// persistent config change.
	if cmd.Model != "" || cmd.Effort != "" {
		return harness.PromptResult{}, fmt.Errorf(
			"%w: opencode has no per-turn model/effort override; use session config",
			harness.ErrUnsupported)
	}
	st := a.state(m.Session.ID)
	if st == nil {
		return harness.PromptResult{}, fmt.Errorf("session %s not tracked by the opencode adapter", m.Session.ID)
	}
	a.mu.Lock()
	if st.turn != nil {
		turnID := st.turnID
		a.mu.Unlock()
		return harness.PromptResult{}, fmt.Errorf("%w: turn %s in flight", harness.ErrBusy, turnID)
	}
	a.mu.Unlock()

	// In flight from the moment Relay accepts it: activity is a sleep blocker
	// for the whole submission.
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

// runtimeID returns the bound generation's actual Runtime.ID, never the key.
func (a *Adapter) runtimeID(sessionID string) string {
	v, ok := a.deps.Supervisor.View(sessionID)
	if !ok {
		return ""
	}
	return v.RuntimeID
}

// attach returns a usable session handle for a prompt, waking the runtime only
// when it must. Exactly one native operation happens:
//
//	live generation already hosting the session → reuse (no protocol call)
//	durable exact native identity             → session/load (exact resume)
//	no identity yet                           → session/new
//
// A new native session is a coherent bind transaction: open/load → apply the
// durable DESIRED configuration through the advertised surface → bind to the
// runtime → persist identity + generation → harness.started. A prompt is only
// ever submitted after the native session is fully configured, so a partially
// configured native session is never exposed as ready.
//
// There is no fallback from a failed exact load to a new session.
func (a *Adapter) attach(ctx context.Context, m *session.Managed, st *sessState) (*acp.Session, error) {
	a.mu.Lock()
	handle, srv, nativeID := st.handle, st.srv, st.nativeID
	a.mu.Unlock()
	if handle != nil && srv != nil {
		return handle, nil
	}

	// A malformed durable identity is rejected BEFORE any runtime is
	// spawned: it must not wake a process at all.
	if nativeID != "" && !harness.ValidNativeID(nativeID) {
		return nil, fmt.Errorf("%w: malformed native session id %q", harness.ErrNativeSessionLost, nativeID)
	}
	cwd := m.Snapshot().Cwd
	desired := desiredConfig(m, harness.ConfigCommand{})
	srv, err := a.ensureRuntime(ctx, cwd)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	if nativeID == "" {
		newHandle, err := acp.OpenSession(callCtx, srv, cwd)
		if err != nil {
			return nil, fmt.Errorf("%w: session/new: %v", harness.ErrRuntimeUnavailable, err)
		}
		return a.finishAttach(callCtx, m, st, srv, newHandle, desired, false)
	}

	loaded, err := acp.AttachSession(callCtx, srv, cwd, nativeID)
	if err != nil {
		// Fail closed: never substitute a fresh session for the exact one.
		return nil, fmt.Errorf("%w: session/load %s: %v", harness.ErrNativeSessionLost, nativeID, err)
	}
	return a.finishAttach(callCtx, m, st, srv, loaded, desired, true)
}

// finishAttach completes the bind transaction for a freshly opened or loaded
// native session, in the exact order the semantics require: apply desired
// config natively → persist identity + generation → bind → harness.started.
func (a *Adapter) finishAttach(ctx context.Context, m *session.Managed, st *sessState,
	srv *acp.Server, handle *acp.Session, desired harness.ConfigCommand, resumed bool) (*acp.Session, error) {
	// Desired configuration is applied natively FIRST: a validation failure
	// must abort the submission before any user turn is sent.
	if err := a.applyDesired(ctx, handle, desired); err != nil {
		handle.Close()
		return nil, err
	}
	// OpenCode returns a durable native identity immediately, so it is
	// persisted now — before any turn — which is what makes a zero-turn
	// cold resume exact later.
	if err := a.recordNative(m, st, handle.ID(), resumed); err != nil {
		handle.Close()
		return nil, err
	}
	if err := a.bind(m.Session.ID, srv, handle); err != nil {
		return nil, err
	}
	// harness.started is published only once the session is bound, so a
	// reader that sees it always observes a ready binding.
	if err := a.publishStarted(m, handle.ID(), resumed); err != nil {
		return nil, err
	}
	// The values the runtime now reports are EFFECTIVE — only after a
	// successful native application do they enter the metrics.
	a.publishMetrics(m, "config", acp.MergeMetrics(a.Metrics(m.Session.ID), observedConfig(handle)))
	return handle, nil
}

// recordNative durably records the exact native identity. It runs only when a
// runtime generation takes ownership of the session, so it always bumps the
// generation:
//
//	creation 0 → first native bind 1 → exact resume into a new generation 2
//
// For OpenCode a returned `ses_*` identity is durable from session/new, so the
// identity is persisted before any turn — which is what makes a zero-turn cold
// resume exact later.
func (a *Adapter) recordNative(m *session.Managed, st *sessState, nativeID string, resumed bool) error {
	if err := a.deps.Materialize(m, harness.SessionUpdate{
		NativeSessionID: &nativeID,
		BumpGeneration:  true,
	}); err != nil {
		return err
	}
	a.mu.Lock()
	first := !st.materialized
	st.nativeID = nativeID
	st.materialized = true
	st.resumed = resumed
	a.mu.Unlock()
	if first {
		_ = a.publishDurable(m, api.EventNativeSession, api.NativeSessionPayload{
			NativeSessionID: nativeID,
			Generation:      m.Snapshot().Generation,
		})
	}
	return nil
}

// publishStarted records that a runtime generation took ownership of the
// session's native session. It is published AFTER the binding exists.
func (a *Adapter) publishStarted(m *session.Managed, nativeID string, resumed bool) error {
	return a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
		RuntimeID:       a.runtimeID(m.Session.ID),
		NativeSessionID: nativeID,
		Model:           m.Snapshot().Model,
		Resumed:         resumed,
	})
}

// completeTurn awaits the terminal turn result and publishes the canonical
// terminal record. The turn stays in flight until that record is durable, so a
// concurrent prompt can never interleave its acceptance events with the
// completion of the previous turn.
func (a *Adapter) completeTurn(m *session.Managed, handle *acp.Session, turn *acp.Turn) {
	stopReason, text, _, usage, err := turn.Result()
	turnID := turn.ID

	// The terminal record and the clearing of the in-flight turn happen under
	// the adapter lock, so a concurrent prompt either sees the turn in flight
	// (and is rejected) or sees it settled with its terminal record already
	// durable — never a window where the record exists but the session is
	// still busy.
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
		// max_tokens / max_turn_requests / refusal and any future reason are
		// recorded as failures rather than reported as success.
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
	_ = handle
}

// Cancel interrupts the in-flight native turn through session/cancel. ACP
// cancel is a notification, so this reports delivery; the terminal
// turn.interrupted record comes from the pending prompt response, never
// synthesized here.
func (a *Adapter) Cancel(_ context.Context, m *session.Managed) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the opencode adapter", m.Session.ID)
	}
	a.mu.Lock()
	handle, turn := st.handle, st.turn
	a.mu.Unlock()
	if handle == nil || turn == nil {
		return fmt.Errorf("%w: session %s has no in-flight turn", harness.ErrNoActiveTurn, m.Session.ID)
	}
	// A cancel is a native mutation: it blocks sleep and serializes against
	// overlapping cancels.
	if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
		return err
	}
	defer a.deps.Supervisor.EndMutation(m.Session.ID)
	if err := handle.Cancel(); err != nil {
		return fmt.Errorf("session/cancel: %w", err)
	}
	return nil
}

// AnswerInput is not implemented for OpenCode: the runtime exposes approvals
// (handled by the permission policy), not Relay's requested-input surface, and
// an approval must never be silently converted into user input.
func (a *Adapter) AnswerInput(_ context.Context, m *session.Managed, _ string, _ []harness.InputAnswer) error {
	return fmt.Errorf("%w: opencode has no requested-input surface; approvals are handled by policy",
		harness.ErrUnsupported)
}
