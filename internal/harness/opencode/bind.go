package opencode

import (
	"context"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// The native bind transaction. A Relay binding is committed only when ALL
// three authorities agree: the supervisor binding exists, the durable Relay
// mutation committed, and the adapter routing is installed. The order is
// strict — apply desired config → Supervisor.Bind → durable commit → routing
// → session.native → harness.started — and every failure before the durable
// commit rolls back synchronously.
//
// A session's durable identity being vendor-resumable (ses_*) is a separate
// fact from Relay having committed the binding: the generation bump rides on
// the durable commit, so a Bind failure can never leave a phantom generation,
// and a commit failure can never leave a phantom supervisor binding.

// finishAttach completes the bind transaction for a freshly opened or loaded
// native session, in the exact order the semantics require:
//
//	apply desired config natively → Supervisor.Bind → durable commit
//	(native identity + generation) → adapter routing → session.native →
//	harness.started.
//
// The durable generation is bumped ONLY after the supervisor binding exists,
// and a failed durable commit rolls the supervisor binding back synchronously
// — a failed transaction leaves neither a phantom generation nor a phantom
// binding.
func (a *Adapter) finishAttach(ctx context.Context, m *session.Managed,
	srv *acp.Server, handle *acp.Session, desired harness.ConfigCommand, resumed bool) (*acp.Session, error) {
	// Desired configuration is applied natively FIRST: a validation failure
	// must abort the submission before any user turn is sent.
	if err := a.applyDesired(ctx, handle, desired); err != nil {
		handle.Close()
		return nil, err
	}
	sessionID, nativeID := m.Session.ID, handle.ID()
	if err := a.deps.Supervisor.Bind(RuntimeKey, sessionID); err != nil {
		handle.Close()
		return nil, fmt.Errorf("%w: %v", harness.ErrRuntimeUnavailable, err)
	}
	// OpenCode returns a durable native identity immediately from
	// session/new, so it is persisted now — before any turn — which is what
	// makes a zero-turn cold resume exact later. The durable mutation also
	// carries the generation bump: creation 0 → first bind 1 → exact resume
	// into a new generation 2.
	if err := a.deps.Materialize(m, harness.SessionUpdate{
		NativeSessionID: &nativeID,
		BumpGeneration:  true,
	}); err != nil {
		// The commit failed: nothing durable changed, so the supervisor
		// binding and the native handle must not remain either.
		a.deps.Supervisor.Unbind(sessionID)
		handle.Close()
		return nil, err
	}
	// The commit stands: mirror the durable identity now — even if the
	// generation dies below, the persisted ses_* is what the next attach
	// must load, never a fresh session/new.
	a.mu.Lock()
	cur := a.sessions[sessionID]
	first := cur != nil && !cur.materialized
	if cur != nil {
		cur.nativeID = nativeID
		cur.materialized = true
		cur.resumed = resumed
	}
	a.mu.Unlock()
	if first {
		_ = a.publishDurable(m, api.EventNativeSession, api.NativeSessionPayload{
			NativeSessionID: nativeID,
			Generation:      m.Snapshot().Generation,
		})
	}
	// The generation may have died mid-transaction (the death path could
	// not see the binding because routing did not exist yet): the durable
	// commit stands, but no routing may claim a dead generation and no
	// start may be advertised for it.
	if _, ok := a.deps.Supervisor.View(sessionID); !ok {
		handle.Close()
		return nil, fmt.Errorf("%w: runtime generation exited during bind", harness.ErrRuntimeUnavailable)
	}
	a.mu.Lock()
	if cur == nil {
		// The session was deleted mid-transaction: roll the committed
		// binding back rather than keep a phantom.
		a.mu.Unlock()
		a.deps.Supervisor.Unbind(sessionID)
		handle.Close()
		return nil, fmt.Errorf("session %s is not tracked by the opencode adapter", sessionID)
	}
	cur.srv = srv
	cur.handle = handle
	a.byNative[nativeID] = sessionID
	a.mu.Unlock()
	// harness.started is published only once the binding is committed, so a
	// reader that sees it always observes a ready binding.
	if err := a.publishStarted(m, nativeID, resumed); err != nil {
		return nil, err
	}
	// The values the runtime now reports are EFFECTIVE — only after a
	// successful native application do they enter the metrics.
	a.publishMetrics(m, "config", acp.MergeMetrics(a.Metrics(m.Session.ID), observedConfig(handle)))
	return handle, nil
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
