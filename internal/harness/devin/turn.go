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
//
// Materialization and generation are orthogonal: the runtime binding already
// bumped the generation, so this write does NOT.
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
