package opencode

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
// RPC. Turn execution is never bounded here — a turn ends through the runtime's
// own prompt response.
const (
	initTimeout = 20 * time.Second
	callTimeout = 30 * time.Second
)

// ensureRuntime returns a ready shared runtime, spawning and initializing one
// when none is live. Concurrent callers converge on ONE process (claim-first
// creation in the supervisor).
func (a *Adapter) ensureRuntime(ctx context.Context, cwd string) (*acp.Server, error) {
	a.mu.Lock()
	if srv, ok := a.servers[RuntimeKey]; ok {
		a.mu.Unlock()
		return srv, nil
	}
	a.mu.Unlock()

	cmd, err := a.deps.Command()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	rtID, err := a.deps.RandRuntimeID()
	if err != nil {
		return nil, err
	}
	var created *acp.Server
	_, err = a.deps.Supervisor.Ensure(ctx, RuntimeKey, session.HarnessOpenCode, rtID, true,
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
				_ = srv.Stop() // the closure owns reaping its own process
				return nil, fmt.Errorf("initialize: %w%s", err, srv.StderrSuffix())
			}
			// Capability check, fail closed: Relay's contract requires exact
			// reattachment, so a runtime that cannot load a session is not a
			// usable OpenCode runtime for Relay.
			if !init.AgentCapabilities.LoadSession {
				_ = srv.Stop()
				return nil, fmt.Errorf("%w: runtime does not advertise session/load", harness.ErrUnsupported)
			}
			a.mu.Lock()
			a.servers[RuntimeKey] = srv
			a.mu.Unlock()
			created = srv
			return srv, nil
		})
	if err != nil {
		if created != nil {
			a.mu.Lock()
			if cur, ok := a.servers[RuntimeKey]; ok && cur == created {
				delete(a.servers, RuntimeKey)
			}
			a.mu.Unlock()
		}
		if errors.Is(err, harness.ErrUnsupported) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	srv, ok := a.servers[RuntimeKey]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: runtime %s has no server", harness.ErrRuntimeUnavailable, RuntimeKey)
	}
	return srv, nil
}

// stopOrphanRuntime reaps a generation a failed bind left behind.
// Called only inside the serialized attach, so no other attach can be
// mid-bind; the supervisor atomically declines when any session is
// bound, so a shared generation is never destroyed.
func (a *Adapter) stopOrphanRuntime() {
	_, _ = a.deps.Supervisor.StopIfUnbound(RuntimeKey)
}

// OnRuntimeGone implements harness.Adapter: the runtime generation is gone, so
// every bound session becomes COLD, in-flight work fails durably, and the
// durable native identity is retained exactly (it is resumable).
func (a *Adapter) OnRuntimeGone(key string, rt *runtime.Runtime, reason string) {
	if key != RuntimeKey {
		return
	}
	a.mu.Lock()
	a.servers = map[string]*acp.Server{}
	type affected struct {
		m    *session.Managed
		turn *acp.Turn
	}
	var (
		ids  []affected
		rtID = rt.ID
	)
	for _, st := range a.sessions {
		if st.handle == nil {
			continue
		}
		if st.nativeID != "" {
			delete(a.byNative, st.nativeID)
		}
		st.srv = nil
		st.handle = nil
		// Snapshot the in-flight turn under the lock: it is the ONLY
		// sessState field the death path may read after unlocking.
		ids = append(ids, affected{
			m:    st.m,
			turn: st.turn,
		})
	}
	a.mu.Unlock()

	for _, e := range ids {
		// The in-flight turn's terminal durable record is owned by
		// completeTurn — exactly once. Fail only settles the ACP turn so
		// that single owner publishes it; the death path never publishes
		// a turn.* record itself.
		if e.turn != nil {
			e.turn.Fail(fmt.Errorf("runtime exited: %s", reason))
		}
		// Logical state first: runtime.exited is the canonical "this
		// generation is gone" record, so a reader that sees it must already
		// observe a settled session.
		_ = a.setSessionState(e.m, session.StateIdle)
		a.deps.Supervisor.SetActivity(e.m.Session.ID, runtime.ActivityIdle)
		_ = a.publishDurable(e.m, api.EventRuntimeExited, api.RuntimeExitedPayload{
			RuntimeID: rtID,
			Reason:    reason,
		})
	}
}

// approve answers an approval request. Relay runs the harness with full
// permissions (the runtime's own approval mode is the user-facing control), so
// the strongest advertised allow option is selected. An unclassifiable request
// fails closed: Relay never approves what it cannot classify, and it never
// converts an approval request into Relay requested-input.
func (a *Adapter) approve(_ string, params acp.RequestPermissionParams) acp.RequestPermissionResult {
	opt, ok := acp.SelectAllowOption(params.Options)
	if !ok {
		return acp.Cancelled()
	}
	return acp.Selected(opt.OptionID)
}

// onUpdate maps one streaming ACP update onto canonical events. Assistant text
// streams as a transient delta (the durable record is the completed message);
// hidden reasoning is deliberately NOT surfaced or persisted; usage updates
// refresh the last-known metrics.
func (a *Adapter) onUpdate(nativeSessionID string, update acp.Update) {
	// ACP notifications carry the NATIVE session id: resolve it to the Relay
	// session it belongs to. An unknown native session (a runtime generation
	// Relay already gave up on) is ignored rather than guessed at.
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
		// Native metadata updates: the adapter's cached surface is refreshed
		// by the ACP handle; Relay's durable model/mode only changes through
		// an accepted Relay config request.
	default:
		// Tool calls, plans, command lists, and any future variant are
		// ignored deliberately rather than guessed at.
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
