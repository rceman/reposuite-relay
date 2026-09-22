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
			// The reader is counted at spawn time — inside the claim-first
			// closure, before Ensure returns — so WaitQuiescent can never
			// miss a live reader, even after OnRuntimeGone forgets the
			// server itself.
			a.wg.Add(1)
			go func() {
				_ = srv.Serve()
				a.wg.Done()
			}()
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
		turnID  string
		hasTurn bool
	}
	a.mu.Lock()
	srv := a.servers[key]
	delete(a.servers, key)
	affected := make([]boundSession, 0, len(a.sessions))
	for _, st := range a.sessions {
		if st.liveKey == key {
			// Snapshot the in-flight turn's identity under the lock: the
			// activeTurn itself keeps mutating under a.mu (prompt writes
			// its turnID, deltas append text), so it must never be
			// dereferenced after the lock is released.
			entry := boundSession{st: st}
			if st.current != nil {
				entry.hasTurn = true
				entry.turnID = st.current.turnID
			}
			affected = append(affected, entry)
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
		st, m := entry.st, entry.st.m
		// Every affected session records the generation's end — a deliberate
		// sleep ends a generation just as surely as a crash does.
		_ = a.publishDurable(m, api.EventRuntimeExited, api.RuntimeExitedPayload{
			RuntimeID: rt.ID,
			Reason:    detail,
		})
		if !unexpected {
			// Deliberate stop: no in-flight turn can exist (it is a sleep
			// blocker), so only local state is released.
			a.mu.Lock()
			st.current = nil
			a.mu.Unlock()
			continue
		}
		if entry.hasTurn {
			_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
				TurnID: entry.turnID,
				Error:  "runtime exited: " + detail,
			})
		}
		unresolved := a.abortInputs(st, "runtime exited: "+detail)
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		// Inputs whose durable terminal record failed to commit are
		// retained: the session stays visibly non-clean (waiting_input
		// still names the unresolved authority) until a retry or the
		// next daemon generation reconciles them.
		if unresolved > 0 {
			a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityWaitingInput)
			_ = a.setSessionState(m, session.StateWaitingInput)
		} else {
			a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
			_ = a.setSessionState(m, session.StateIdle)
		}
	}
}

// abortInputs terminates every unresolved input durably, coordinating
// with the per-input resolution phase:
//
//   - pending: never crossed the native commit point → publish
//     input.aborted; the entry is dropped only after the durable record
//     commits. A failed append retains the entry (marked wantAbort) so a
//     later sweep or restart reconciliation can retry — the durable
//     outcome commits BEFORE live pending authority is discarded.
//   - answered: the native answer already crossed its commit point →
//     publish the retained sanitized input.resolved instead; a native
//     answer is never rewritten into the contradictory input.aborted.
//   - responding/committing/aborting: a side effect owns the input →
//     mark wantAbort; the owner reconciles after its write returns
//     (failed Respond → abort, committed/failed resolution → answered
//     or deleted).
//
// The return value counts inputs whose durable terminal record is still
// uncommitted after the sweep — the caller must keep the session visibly
// non-clean (waiting_input) until authority converges.
func (a *Adapter) abortInputs(st *sessState, reason string) int {
	a.mu.Lock()
	ids := make([]string, 0, len(st.inputs))
	for id := range st.inputs {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	unresolved := 0
	for _, id := range ids {
		a.mu.Lock()
		pi, ok := st.inputs[id]
		if !ok {
			a.mu.Unlock()
			continue
		}
		var publish func() error
		revert := pi.phase
		switch pi.phase {
		case inputPending:
			// Retries a retained wantAbort entry too — a new sweep is a
			// fresh chance to land the durable record.
			pi.phase = inputAborting
			publish = func() error {
				return a.publishDurable(st.m, api.EventInputAborted, inputAbortedPayload{
					InputID: id,
					Reason:  reason,
				})
			}
		case inputAnswered:
			pi.phase = inputCommitting
			publish = func() error {
				return a.publishDurable(st.m, api.EventInputResolved, pi.resolution)
			}
		default:
			// A side effect owns the input; it reconciles on return.
			pi.wantAbort = true
			pi.abortReason = reason
			unresolved++
			a.mu.Unlock()
			continue
		}
		a.mu.Unlock()
		if err := publish(); err != nil {
			a.mu.Lock()
			pi.phase = revert
			pi.wantAbort = true
			pi.abortReason = reason
			a.mu.Unlock()
			unresolved++
			continue
		}
		a.mu.Lock()
		delete(st.inputs, id)
		a.mu.Unlock()
	}
	return unresolved
}

// setSessionState durably records the logical session state. The
// no-change check lives in the daemon's Materialize hook, under MetaMu:
// reading durable metadata here would race with the adapter's own
// notification goroutines.
func (a *Adapter) setSessionState(m *session.Managed, state string) error {
	return a.deps.Materialize(m, harness.SessionUpdate{State: &state})
}

// --- prompt / turn --------------------------------------------------
