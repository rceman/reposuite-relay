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

	handle, err := a.attach(ctx, m, st)
	if err != nil {
		return fail(err)
	}
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
		RuntimeID:       RuntimeKey,
	}, nil
}

// attach returns a usable session handle for a prompt, waking the runtime only
// when it must. Exactly one native operation happens:
//
//	live generation already hosting the session → reuse (no protocol call)
//	durable exact native identity             → session/load (exact resume)
//	no identity yet                           → session/new
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
		// OpenCode returns a durable native identity immediately, so it is
		// persisted now — before any turn — which is what makes a zero-turn
		// cold resume exact later.
		if err := a.persistNative(m, st, newHandle.ID(), false); err != nil {
			newHandle.Close()
			return nil, err
		}
		if err := a.bind(m.Session.ID, srv, newHandle); err != nil {
			return nil, err
		}
		return newHandle, nil
	}

	loaded, err := acp.AttachSession(callCtx, srv, cwd, nativeID)
	if err != nil {
		// Fail closed: never substitute a fresh session for the exact one.
		return nil, fmt.Errorf("%w: session/load %s: %v", harness.ErrNativeSessionLost, nativeID, err)
	}
	if err := a.persistNative(m, st, loaded.ID(), true); err != nil {
		loaded.Close()
		return nil, err
	}
	if err := a.bind(m.Session.ID, srv, loaded); err != nil {
		return nil, err
	}
	return loaded, nil
}

// persistNative records the exact native identity durably and publishes
// harness.started. It runs only when a runtime generation takes ownership of
// the session, so it always bumps the generation:
//
//	creation 0 → first native bind 1 → exact resume into a new generation 2
//
// For OpenCode a returned `ses_*` identity is durable from session/new, so the
// identity is persisted before any turn — which is what makes a zero-turn cold
// resume exact later.
func (a *Adapter) persistNative(m *session.Managed, st *sessState, nativeID string, resumed bool) error {
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
	return a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
		RuntimeID:       RuntimeKey,
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
