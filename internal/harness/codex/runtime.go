package codex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// ensureRuntime returns a ready shared app-server, spawning and
// initializing one when none is live. Concurrent callers converge on one
// process (the supervisor owns that convergence).
func (a *Adapter) ensureRuntime(ctx context.Context, cwd string) (*Server, error) {
	a.mu.Lock()
	if srv, ok := a.servers[RuntimeKey]; ok {
		a.mu.Unlock()
		return srv, nil
	}
	a.mu.Unlock()

	cmd, err := a.deps.Command()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	rtID, err := a.deps.RandRuntimeID()
	if err != nil {
		return nil, err
	}
	// The spawn closure owns the whole generation handshake: spawn the
	// process, serve the connection, initialize the app-server, install
	// handlers. It returns only when the runtime is USABLE, so concurrent
	// wake-ups converge on one process and never observe a half-initialized
	// server (claim-first creation in the supervisor).
	var created *Server
	_, err = a.deps.Supervisor.Ensure(ctx, RuntimeKey, session.HarnessCodex, rtID, true,
		func() (runtime.Harness, error) {
			srv, err := StartServer(cmd, cwd)
			if err != nil {
				return nil, err
			}
			go srv.Serve()
			initCtx, cancel := context.WithTimeout(ctx, initTimeout)
			defer cancel()
			if _, err := srv.Initialize(initCtx, ClientInfo{
				Name:    "reposuite-relay",
				Title:   "RepoSuite Relay",
				Version: a.deps.Version,
			}); err != nil {
				_ = srv.Stop() // the closure owns reaping its own process
				return nil, fmt.Errorf("initialize: %w%s", err, stderrSuffix(srv))
			}
			a.installHandlers(srv)
			a.mu.Lock()
			a.servers[RuntimeKey] = srv
			a.mu.Unlock()
			created = srv
			return srv, nil
		})
	if err != nil {
		if created != nil {
			// The generation was stopped while spawning: drop our entry so
			// no later caller can pick up a dead server.
			a.mu.Lock()
			if cur, ok := a.servers[RuntimeKey]; ok && cur == created {
				delete(a.servers, RuntimeKey)
			}
			a.mu.Unlock()
		}
		return nil, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	a.mu.Lock()
	srv, ok := a.servers[RuntimeKey]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: runtime %s has no server", ErrRuntimeUnavailable, RuntimeKey)
	}
	return srv, nil
}

const initTimeout = 15 * time.Second

// callTimeout bounds a single control RPC (thread start/resume, turn
// start, interrupt). Turn execution itself is not bounded here — the
// turn completes via notifications.
const callTimeout = 30 * time.Second

func stderrSuffix(srv *Server) string {
	tail := srv.Stderr()
	if tail == "" {
		return ""
	}
	return " (stderr: " + tail + ")"
}

// installHandlers wires notification and server-request handling once
// per app-server process.
func (a *Adapter) installHandlers(srv *Server) {
	conn := srv.Conn()
	conn.SetNotificationHandler(a.handleNotification)
	conn.SetHandler(MethodRequestUserInput, a.handleRequestUserInput)
	conn.SetHandler(MethodCommandApproval, a.handleApproval)
	conn.SetHandler(MethodFileApproval, a.handleApproval)
	conn.SetHandler(MethodPermissions, a.handleApproval)
}

// OnRuntimeGone is the supervisor's runtime-removal notification: it fires
// exactly once per runtime generation, whether the process died on its own
// or Relay stopped it deliberately. The adapter must never keep serving a
// server whose process is gone.
//
//   - an unexpected exit fails in-flight work durably (turn.failed), aborts
//     unresolved requested input, and records runtime.exited;
//   - a deliberate stop (idle sleep, session stop, shutdown) has no
//     in-flight work by construction, so nothing durable is published.
//
// Either way every bound session becomes COLD: the in-memory thread died
// with the process, while a materialized session keeps its exact durable
// native identity for the next resume.
func (a *Adapter) OnRuntimeGone(key string, rt *runtime.Runtime, reason string) {
	type boundSession struct {
		st      *sessState
		current *activeTurn
	}
	a.mu.Lock()
	srv := a.servers[key]
	delete(a.servers, key)
	affected := make([]boundSession, 0, len(a.sessions))
	for _, st := range a.sessions {
		if st.liveKey == key {
			// Capture the in-flight turn under the lock: it is the turn
			// this generation owned, and it must not be re-read after a
			// concurrent prompt has started the next generation.
			affected = append(affected, boundSession{
				st:      st,
				current: st.current,
			})
		}
	}
	for _, entry := range affected {
		st := entry.st
		st.liveKey = ""
		st.nativeID = "" // the in-memory thread died with the process
		if st.materialized {
			// The durable exact identity survives; a rebind resumes it.
			st.nativeID = st.m.Snapshot().NativeSessionID
		}
	}
	a.mu.Unlock()

	unexpected := reason != runtime.ReasonStopped
	detail := reason
	if unexpected && srv != nil {
		if err := srv.ExitErr(); err != nil && !errors.Is(err, ErrConnClosed) {
			detail = err.Error()
		}
	}
	for _, entry := range affected {
		st, current := entry.st, entry.current
		m := st.m
		if !unexpected {
			// Deliberate stop: no in-flight turn can exist (it is a sleep
			// blocker), so only local state is released.
			a.mu.Lock()
			st.current = nil
			a.mu.Unlock()
			continue
		}
		_ = a.publishDurable(m, api.EventRuntimeExited, runtimeExitedPayload{
			RuntimeID: rt.ID,
			Reason:    detail,
		})
		if current != nil {
			_ = a.publishDurable(m, api.EventTurnFailed, turnEventPayload{
				TurnID: current.turnID,
				Error:  "runtime exited: " + detail,
			})
		}
		a.abortInputs(st, "runtime exited: "+detail)
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
	}
}

// abortInputs publishes input.aborted for every unresolved request and
// drops the mapping (a dead runtime can never answer them).
func (a *Adapter) abortInputs(st *sessState, reason string) {
	a.mu.Lock()
	pending := st.inputs
	st.inputs = map[string]*pendingInput{}
	a.mu.Unlock()
	for id := range pending {
		_ = a.publishDurable(st.m, api.EventInputAborted, inputAbortedPayload{
			InputID: id,
			Reason:  reason,
		})
	}
}

// setSessionState durably records the logical session state. The
// no-change check lives in the daemon's Materialize hook, under MetaMu:
// reading durable metadata here would race with the adapter's own
// notification goroutines.
func (a *Adapter) setSessionState(m *session.Managed, state string) error {
	return a.deps.Materialize(m, harness.SessionUpdate{State: &state})
}

// --- prompt / turn --------------------------------------------------
