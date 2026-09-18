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
			go srv.Serve()
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

// bind attaches a session to a live runtime generation.
func (a *Adapter) bind(sessionID string, srv *acp.Server, handle *acp.Session) error {
	if err := a.deps.Supervisor.Bind(RuntimeKey, sessionID); err != nil {
		handle.Close()
		return fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	st := a.sessions[sessionID]
	if st == nil {
		a.mu.Unlock()
		handle.Close()
		a.deps.Supervisor.Unbind(sessionID)
		return fmt.Errorf("session %s is not tracked by the opencode adapter", sessionID)
	}
	st.srv = srv
	st.handle = handle
	st.nativeID = handle.ID()
	a.byNative[handle.ID()] = sessionID
	a.mu.Unlock()
	return nil
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
		st *sessState
		m  *session.Managed
	}
	var (
		ids      []affected
		rtID     = rt.ID
		inFlight []affected
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
		ids = append(ids, affected{
			st: st,
			m:  st.m,
		})
		if st.turn != nil {
			inFlight = append(inFlight, affected{
				st: st,
				m:  st.m,
			})
		}
	}
	a.mu.Unlock()

	// A dead runtime cannot answer: the in-flight turn fails durably and the
	// session goes COLD with its exact native identity intact.
	for _, e := range inFlight {
		_ = a.publishDurable(e.m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: e.st.turnID,
			Error:  "runtime exited: " + reason,
		})
		a.deps.Supervisor.SetActivity(e.m.Session.ID, runtime.ActivityIdle)
	}
	for _, e := range ids {
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
		a.publishMetrics(m, "context", contextMetrics(update))
	case acp.UpdateConfigOption, acp.UpdateCurrentMode, acp.UpdateSessionInfo:
		// Native metadata updates: the adapter's cached surface is refreshed
		// by the ACP handle; Relay's durable model/mode only changes through
		// an accepted Relay config request.
	default:
		// Tool calls, plans, command lists, and any future variant are
		// ignored deliberately rather than guessed at.
	}
}

// contextMetrics projects a native usage_update onto the canonical shape. A
// missing measurement stays absent — never reported as zero.
func contextMetrics(u acp.Update) *harness.SessionMetrics {
	if u.Used == nil && u.Size == nil {
		return nil
	}
	out := &harness.SessionMetrics{}
	if u.Used != nil {
		out.ContextUsed = harness.Int64(*u.Used)
	}
	if u.Size != nil {
		out.ContextLimit = harness.Int64(*u.Size)
	}
	return out
}

// mergeMetrics overlays non-nil measurements onto the last-known set so a
// later partial update does not erase earlier fields.
func mergeMetrics(base *harness.SessionMetrics, next *harness.SessionMetrics) *harness.SessionMetrics {
	if base == nil {
		return next
	}
	if next == nil {
		return base
	}
	out := *base
	if next.Model != "" {
		out.Model = next.Model
	}
	if next.Mode != "" {
		out.Mode = next.Mode
	}
	if next.ContextUsed != nil {
		out.ContextUsed = next.ContextUsed
	}
	if next.ContextLimit != nil {
		out.ContextLimit = next.ContextLimit
	}
	if next.InputTokens != nil {
		out.InputTokens = next.InputTokens
	}
	if next.OutputTokens != nil {
		out.OutputTokens = next.OutputTokens
	}
	if next.ReasoningTokens != nil {
		out.ReasoningTokens = next.ReasoningTokens
	}
	if next.CacheTokens != nil {
		out.CacheTokens = next.CacheTokens
	}
	if next.QuotaUsed != nil {
		out.QuotaUsed = next.QuotaUsed
	}
	if next.QuotaLimit != nil {
		out.QuotaLimit = next.QuotaLimit
	}
	if next.ResetAt != "" {
		out.ResetAt = next.ResetAt
	}
	return &out
}

// usageMetrics projects native prompt usage onto the canonical shape.
func usageMetrics(u *acp.Usage) *harness.SessionMetrics {
	if u == nil {
		return nil
	}
	out := &harness.SessionMetrics{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		ReasoningTokens: u.ThoughtTokens,
	}
	if u.CachedReadTokens != nil {
		out.CacheTokens = u.CachedReadTokens
	} else if u.CachedWriteTokens != nil {
		out.CacheTokens = u.CachedWriteTokens
	}
	return out
}

// marshal encodes a canonical event payload.
func marshal(payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode event payload: %w", err)
	}
	return raw, nil
}
