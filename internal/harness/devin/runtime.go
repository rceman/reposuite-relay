package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// Bounds. initTimeout covers the ACP handshake; callTimeout covers one control
// RPC. Turn execution is never bounded here.
const (
	initTimeout = 20 * time.Second
	callTimeout = 30 * time.Second
)

// ensureRuntime returns a ready runtime generation for a process-level model,
// spawning and initializing one when none is live. The ACP handshake and the
// explicit bypass-mode selection both happen inside the spawn closure, so the
// generation is only published once it is USABLE.
func (a *Adapter) ensureRuntime(ctx context.Context, cwd, model string) (*acp.Server, string, error) {
	key := RuntimeKeyFor(model)
	a.mu.Lock()
	if srv, ok := a.servers[key]; ok {
		a.mu.Unlock()
		return srv, key, nil
	}
	a.mu.Unlock()

	cmd, err := a.deps.Command(model)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	rtID, err := a.deps.RandRuntimeID()
	if err != nil {
		return nil, "", err
	}
	var created *acp.Server
	_, err = a.deps.Supervisor.Ensure(ctx, key, session.HarnessDevin, rtID, true,
		func() (runtime.Harness, error) {
			srv, err := acp.StartServer(cmd, cwd)
			if err != nil {
				return nil, err
			}
			// The reader is counted at spawn time — inside the claim-first
			// closure, before Ensure returns — so WaitQuiescent can never
			// miss a live reader, even after OnRuntimeGone forgets the
			// server itself.
			a.wg.Add(1)
			go func() {
				_ = srv.Serve()
				a.wg.Done()
			}()
			srv.Configure(acp.ServerConfig{
				Client: acp.ClientInfo{
					Name:    "reposuite-relay",
					Title:   "RepoSuite Relay",
					Version: a.deps.Version,
				},
				Approve:  a.approve,
				OnUpdate: a.onUpdate,
			})
			initCtx, cancel := context.WithTimeout(ctx, initTimeout)
			defer cancel()
			init, err := srv.Initialize(initCtx, acp.ClientInfo{
				Name:    "reposuite-relay",
				Title:   "RepoSuite Relay",
				Version: a.deps.Version,
			})
			if err != nil {
				_ = srv.Stop()
				return nil, fmt.Errorf("initialize: %w%s", err, srv.StderrSuffix())
			}
			// Capability check, fail closed: Relay's contract requires exact
			// reattachment, so a runtime that cannot load a session is not a
			// usable Devin runtime for Relay.
			if !init.AgentCapabilities.LoadSession {
				_ = srv.Stop()
				return nil, fmt.Errorf("%w: runtime does not advertise session/load", harness.ErrUnsupported)
			}
			a.mu.Lock()
			a.servers[key] = srv
			a.mu.Unlock()
			created = srv
			return srv, nil
		})
	if err != nil {
		if created != nil {
			a.mu.Lock()
			if cur, ok := a.servers[key]; ok && cur == created {
				delete(a.servers, key)
			}
			a.mu.Unlock()
		}
		if errors.Is(err, harness.ErrUnsupported) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	srv, ok := a.servers[key]
	a.mu.Unlock()
	if !ok {
		return nil, "", fmt.Errorf("%w: runtime %s has no server", harness.ErrRuntimeUnavailable, key)
	}
	return srv, key, nil
}

// stopOrphanRuntime reaps a generation a failed bind left behind: a
// runtime with zero bound sessions can only be the one this attach just
// spawned — stopping it frees the provider-side resources it holds
// (Devin ACP holds a per-process session lock; an orphaned server
// poisons every later session/load with session_locked). A shared
// generation with bound sessions is never touched.
func (a *Adapter) stopOrphanRuntime(key string) {
	if rt, ok := a.deps.Supervisor.Get(key); ok && len(rt.Sessions()) == 0 {
		_ = a.deps.Supervisor.StopIfIdle(key)
	}
}

// bind attaches a session to a live runtime generation.
func (a *Adapter) bind(sessionID, runtimeKey string, srv *acp.Server, handle *acp.Session) error {
	if err := a.deps.Supervisor.Bind(runtimeKey, sessionID); err != nil {
		handle.Close()
		return fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	st := a.sessions[sessionID]
	if st == nil {
		a.mu.Unlock()
		handle.Close()
		a.deps.Supervisor.Unbind(sessionID)
		return fmt.Errorf("session %s is not tracked by the devin adapter", sessionID)
	}
	st.srv = srv
	st.handle = handle
	st.liveRuntimeKey = runtimeKey
	st.nativeID = handle.ID()
	a.byNative[handle.ID()] = sessionID
	a.mu.Unlock()
	return nil
}

// OnRuntimeGone implements harness.Adapter: the generation is gone, so every
// session bound to THAT generation becomes COLD, in-flight work fails durably,
// and a proven durable slug is retained exactly.
//
// A slug that was never proven (no completed turn) is NOT retained: a
// zero-turn Devin session cannot be resumed, so claiming that identity would be
// a lie the next prompt would have to break.
//
// Only sessions bound to the dead key are affected: a session that migrated to
// a different process-model generation must not be disturbed by the death of
// the generation it left.
func (a *Adapter) OnRuntimeGone(key string, rt *runtime.Runtime, reason string) {
	a.mu.Lock()
	delete(a.servers, key)
	type affected struct {
		m    *session.Managed
		turn *acp.Turn
	}
	var affectedSessions []affected
	for _, st := range a.sessions {
		if st.handle == nil || st.liveRuntimeKey != key {
			continue
		}
		if st.nativeID != "" {
			delete(a.byNative, st.nativeID)
		}
		st.srv = nil
		st.handle = nil
		st.liveRuntimeKey = ""
		if !st.materialized {
			// Drop the unproven identity: it must not survive a generation.
			st.nativeID = ""
		}
		// Snapshot the turn under the lock: it is the ONLY sessState field
		// the death path may read after unlocking.
		affectedSessions = append(affectedSessions, affected{
			m:    st.m,
			turn: st.turn,
		})
	}
	a.mu.Unlock()

	for _, e := range affectedSessions {
		// The in-flight turn's terminal durable record is owned by
		// completeTurn — exactly once. Fail only settles the ACP turn so
		// that single owner publishes it; the death path never publishes
		// a turn.* record itself.
		if e.turn != nil {
			e.turn.Fail(fmt.Errorf("runtime exited: %s", reason))
		}
		_ = a.setSessionState(e.m, session.StateIdle)
		a.deps.Supervisor.SetActivity(e.m.Session.ID, runtime.ActivityIdle)
		_ = a.publishDurable(e.m, api.EventRuntimeExited, api.RuntimeExitedPayload{
			RuntimeID: rt.ID,
			Reason:    reason,
		})
	}
}

// approve answers an approval request. Relay runs the harness with full
// permissions (the native approval mode is the user-facing control), so the
// strongest advertised allow option is selected. An unclassifiable request
// fails closed.
func (a *Adapter) approve(_ string, params acp.RequestPermissionParams) acp.RequestPermissionResult {
	opt, ok := acp.SelectAllowOption(params.Options)
	if !ok {
		return acp.Cancelled()
	}
	return acp.Selected(opt.OptionID)
}

// onUpdate maps one streaming ACP update onto canonical events, resolving the
// NATIVE slug the notification carries to its Relay session.
func (a *Adapter) onUpdate(nativeSessionID string, update acp.Update) {
	st, ok := a.relaySessionFor(nativeSessionID)
	if !ok {
		return
	}
	a.mu.Lock()
	m, turnID := st.m, st.turnID
	a.mu.Unlock()
	if m == nil {
		return
	}
	switch update.SessionUpdate {
	case acp.UpdateAgentMessageChunk:
		_ = a.publishTransient(m, api.EventMessageAgentDelta, api.MessageCompletedPayload{
			TurnID: turnID,
			Text:   update.Text(),
		})
	case acp.UpdateUsage:
		// Merge onto the last-known set: a partial native measurement must
		// never erase a value the runtime reported earlier.
		a.publishMetrics(m, "context", acp.MergeMetrics(
			a.Metrics(m.Session.ID), acp.ContextMetrics(update)))
	case acp.UpdateConfigOption, acp.UpdateCurrentMode, acp.UpdateSessionInfo:
		// The ACP handle refreshes its cached surface; Relay's durable
		// model/mode only changes through an accepted Relay config request.
	default:
		// Tool calls, plans, and any future variant are ignored deliberately.
	}
}

// marshal encodes a canonical event payload.
func marshal(payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode event payload: %w", err)
	}
	return raw, nil
}
