package codex

import (
	"context"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
)

// ConfigCommand is an accepted configuration change.
type ConfigCommand struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// ApplyConfig records an accepted configuration change. The verified
// app-server surface applies model/mode at thread start/resume and, for
// model and effort, per turn (turn/start). There is no verified
// thread-level model mutation while a thread is live, so a change takes
// effect on the next turn or the next resume — Relay records the
// accepted configuration rather than pretending otherwise. A change
// during an in-flight turn is accepted and applies to the next turn: the
// running turn already captured its own model/effort.
func (a *Adapter) ApplyConfig(_ context.Context, m *session.Managed, cmd ConfigCommand) error {
	if a.state(m.Session.ID) == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	if snap := m.Snapshot(); snap.Model == cmd.Model && snap.Mode == cmd.Mode {
		return nil
	}
	if err := a.deps.Materialize(m, SessionUpdate{
		Model: &cmd.Model,
		Mode:  &cmd.Mode,
	}); err != nil {
		return err
	}
	return a.publishDurable(m, api.EventConfigChanged, ConfigCommand{
		Model: cmd.Model,
		Mode:  cmd.Mode,
	})
}

// StopSession releases adapter state for a session that is being deleted:
// an in-flight turn is interrupted (best effort — a deleted session must
// not leave a native turn running), unresolved input is aborted,
// live-thread tracking is dropped, and the shared runtime keeps serving
// other sessions.
func (a *Adapter) StopSession(m *session.Managed) {
	st := a.state(m.Session.ID)
	if st == nil {
		return
	}
	a.mu.Lock()
	cur := st.current
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if cur != nil && cur.turnID != "" && srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		if err := srv.TurnInterrupt(ctx, TurnInterruptParams{
			ThreadID: cur.nativeThreadID,
			TurnID:   cur.turnID,
		}); err != nil {
			diag("interrupt on stop of session %s: %v", m.Session.Key, err)
		}
		cancel()
	}
	a.abortInputs(st, "session stopped")
	a.mu.Lock()
	if st.nativeID != "" {
		delete(a.threads, st.nativeID)
	}
	st.liveKey = ""
	st.current = nil
	a.mu.Unlock()
	a.Forget(m.Session.ID)
}

// Idle reports whether a session has no in-flight native work.
func (a *Adapter) Idle(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return true
	}
	return st.current == nil && len(st.inputs) == 0
}

// DebugState is a diagnostic snapshot (never persisted).
type DebugState struct {
	NativeThreadID string
	Live           bool
	Materialized   bool
	PendingInputs  int
}

// State reports adapter state for tests and diagnostics.
func (a *Adapter) State(sessionID string) DebugState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return DebugState{}
	}
	return DebugState{
		NativeThreadID: st.nativeID,
		Live:           st.liveKey != "",
		Materialized:   st.materialized,
		PendingInputs:  len(st.inputs),
	}
}
