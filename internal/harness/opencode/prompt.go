package opencode

import (
	"context"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/telemetry"
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

	a.spawnTurn(m, handle, turn)
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
	// Serialize: Supervisor.Bind is only ever reached inside attach, so
	// holding attachMu for the whole attach means a failing attach's
	// orphan cleanup can never race another attach's pre-bind work.
	a.attachMu.Lock()
	defer a.attachMu.Unlock()
	if a.deps.AttachGate != nil {
		a.deps.AttachGate()
	}
	a.mu.Lock()
	handle, srv, nativeID := st.handle, st.srv, st.nativeID
	a.mu.Unlock()
	if handle != nil && srv != nil {
		if _, ok := a.deps.Supervisor.View(m.Session.ID); ok {
			return handle, nil
		}
		// The generation hosting this handle died while the death path
		// could not see the binding (the session was mid-bind): the
		// routing is stale, so drop it and re-bind exactly.
		a.mu.Lock()
		if st.handle == handle {
			st.srv = nil
			st.handle = nil
		}
		if cur, ok := a.byNative[handle.ID()]; ok && cur == m.Session.ID {
			delete(a.byNative, handle.ID())
		}
		a.mu.Unlock()
		handle.Close()
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
			a.stopOrphanRuntime()
			return nil, fmt.Errorf("%w: session/new: %v", harness.ErrRuntimeUnavailable, err)
		}
		h, err := a.finishAttach(callCtx, m, srv, newHandle, desired, false)
		if err != nil {
			a.stopOrphanRuntime()
		}
		return h, err
	}

	loaded, err := acp.AttachSession(callCtx, srv, cwd, nativeID)
	if err != nil {
		// Fail closed: never substitute a fresh session for the exact one.
		a.stopOrphanRuntime()
		return nil, fmt.Errorf("%w: session/load %s: %v", harness.ErrNativeSessionLost, nativeID, err)
	}
	h, err := a.finishAttach(callCtx, m, srv, loaded, desired, true)
	if err != nil {
		a.stopOrphanRuntime()
	}
	return h, err
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
		// final_answer: the runtime-visible completed output, verbatim.
		a.deps.Telemetry.FinalAnswer(m, text)
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
	// Turn-scoped usage telemetry: provider-authoritative values copied
	// exactly; a turn is not a proven single model call so the canonical
	// event carries usage_estimated.
	if usage != nil {
		a.deps.Telemetry.ModelUsage(m, telemetry.Usage{
			CallID:   "turn:" + turnID,
			Status:   stopReason,
			Input:    usage.InputTokens,
			Output:   usage.OutputTokens,
			CachedIn: acp.FirstToken(usage.CachedReadTokens, usage.CachedWriteTokens),
			Reason:   usage.ThoughtTokens,
		})
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
