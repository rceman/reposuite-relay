package runtime

import (
	"context"
	"errors"
	"github.com/rceman/reposuite-relay/internal/session"
	"sort"
	"sync"
	"time"
)

// Activity is the per-session turn state. It is deliberately separate
// from the (possibly shared) runtime process state: five sessions on one
// Codex app-server each have their own activity while sharing one PID.
type Activity = string

const (
	// ActivityIdle means the session has no in-flight native work.
	ActivityIdle Activity = "idle"
	// ActivityActive means a native turn is in flight.
	ActivityActive Activity = "active"
	// ActivityWaitingInput means a native requested-input request is
	// unresolved — a mandatory sleep blocker.
	ActivityWaitingInput Activity = "waiting_input"
)

// Stable supervisor errors.
var (
	// ErrRuntimeBusy means a sleep/stop request was refused because the
	// runtime has active work (turn, pending input, in-flight mutation).
	ErrRuntimeBusy = errors.New("runtime busy")
	// ErrRuntimeGone means the runtime key is unknown (never started, or
	// already stopped/exited).
	ErrRuntimeGone = errors.New("runtime not present")
	// ErrClosed means the supervisor itself is closed (daemon shutdown).
	ErrClosed = errors.New("runtime supervisor closed")
)

// Harness is one owned harness process tree. Process satisfies it; test
// fakes may too.
type Harness interface {
	PID() int
	// Wait closes when the process is terminated and reaped.
	Wait() <-chan struct{}
	// Stop terminates the whole tree with bounded phases and confirms
	// reaping.
	Stop() error
}

// SpawnFunc creates a harness process for a runtime key.
type SpawnFunc func() (Harness, error)

// Runtime is one ephemeral harness process generation.
type Runtime struct {
	ID        string // ephemeral runtime identity (never persisted)
	Key       string // supervisor key: "codex/default", "fixture/<sessionID>"
	Kind      string // session.HarnessFixture | session.HarnessCodex
	Shared    bool   // shared runtimes survive individual session deletes
	PID       int
	StartedAt time.Time
	State     string // session.Runtime* constant

	h         Harness
	abandoned bool          // the claim was stopped while spawning
	ready     chan struct{} // closed once the claim resolves (ready or gone)
	readyDone bool

	sessions  map[string]struct{} // bound RelaySession IDs
	activity  map[string]Activity
	mutations map[string]struct{} // in-flight native mutations (cancel)
}

// resolveReadyLocked releases every waiter on this claim exactly once.
// Caller holds Supervisor.mu.
func (r *Runtime) resolveReadyLocked() {
	if !r.readyDone {
		r.readyDone = true
		close(r.ready)
	}
}

// View is the read-only per-session runtime projection.
type View struct {
	RuntimeID string
	Kind      string
	State     string
	PID       int
	StartedAt time.Time
	Activity  Activity
}

// Sessions returns the bound RelaySession IDs (sorted, deterministic).
func (r *Runtime) Sessions() []string {
	out := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Supervisor owns every runtime process and every session<->runtime
// binding. It is the ephemeral process authority; durable session
// authority stays in the registry/store.
type Supervisor struct {
	mu        sync.Mutex
	runtimes  map[string]*Runtime // by runtime key
	bySession map[string]string   // RelaySession ID -> runtime key
	closed    bool

	// watchWg counts live runtime watch goroutines. It is incremented at
	// claim time (inside Ensure, before any waiter can observe the claim),
	// so WaitWatchers can never miss a watcher that still exists.
	watchWg sync.WaitGroup

	// OnGone is called (without the lock) exactly once per runtime, after
	// its process tree is gone — whether it exited on its own or was
	// stopped deliberately. The daemon wires it to adapter cleanup, which
	// must never keep serving a runtime whose process is gone.
	OnGone func(key string, rt *Runtime, reason string)
}

// New returns an empty supervisor.
func New() *Supervisor {
	return &Supervisor{
		runtimes:  map[string]*Runtime{},
		bySession: map[string]string{},
	}
}

// Ensure returns the runtime for key, creating it when absent.
//
// Creation is claim-first: the first caller publishes a `starting`
// placeholder and spawns outside the lock; concurrent callers WAIT for
// that generation to become ready (or disappear) instead of starting a
// second process. A cold runtime therefore never spawns twice, however
// many sessions wake at once.
//
// The spawn closure must return only when the runtime is USABLE — process
// started and protocol initialized. If it returns an error the claim is
// released and waiters observe the failure; the closure owns reaping any
// process it started.
func (s *Supervisor) Ensure(ctx context.Context, key, kind, newID string, shared bool, spawn SpawnFunc) (*Runtime, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, ErrClosed
		}
		if rt, ok := s.runtimes[key]; ok {
			if rt.State != session.RuntimeStarting {
				s.mu.Unlock()
				return rt, nil
			}
			ready := rt.ready
			s.mu.Unlock()
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue // re-check: ready, gone, or claim it ourselves
		}
		rt := &Runtime{
			ID:        newID,
			Key:       key,
			Kind:      kind,
			Shared:    shared,
			State:     session.RuntimeStarting,
			ready:     make(chan struct{}),
			sessions:  map[string]struct{}{},
			activity:  map[string]Activity{},
			mutations: map[string]struct{}{},
		}
		// The claim owns one future watcher: counted before it exists so a
		// WaitWatchers caller can never slip between spawn and watch start.
		s.watchWg.Add(1)
		s.runtimes[key] = rt
		s.mu.Unlock()

		h, err := spawn()
		s.mu.Lock()
		if err != nil {
			if s.runtimes[key] == rt {
				delete(s.runtimes, key)
			}
			rt.resolveReadyLocked()
			s.mu.Unlock()
			s.watchWg.Done() // no watcher will run
			return nil, err
		}
		if rt.abandoned || s.runtimes[key] != rt {
			// Stopped or closed while spawning: never leak the process.
			s.mu.Unlock()
			_ = h.Stop()
			s.watchWg.Done() // no watcher will run
			return nil, ErrRuntimeGone
		}
		rt.h = h
		rt.PID = h.PID()
		rt.StartedAt = time.Now()
		rt.State = session.RuntimeWarm
		rt.resolveReadyLocked()
		s.mu.Unlock()
		go s.watch(rt)
		return rt, nil
	}
}

// watch observes the runtime process and reacts to an unexpected exit.
func (s *Supervisor) watch(rt *Runtime) {
	defer s.watchWg.Done()
	<-rt.h.Wait()
	s.mu.Lock()
	if cur, ok := s.runtimes[rt.Key]; !ok || cur != rt {
		s.mu.Unlock()
		return // already removed by an explicit stop
	}
	delete(s.runtimes, rt.Key)
	for id := range rt.sessions {
		if s.bySession[id] == rt.Key {
			delete(s.bySession, id)
		}
	}
	onGone := s.OnGone
	s.mu.Unlock()
	if onGone != nil {
		onGone(rt.Key, rt, ReasonExited)
	}
}

// WaitWatchers blocks until every runtime watch goroutine has exited —
// i.e. every OnGone callback (and whatever cleanup it performs) has fully
// returned. Callers that tear down state a watcher might still touch (test
// TempDirs, closing stores) use this to quiesce the supervisor first.
func (s *Supervisor) WaitWatchers() {
	s.watchWg.Wait()
}

// Reasons reported to OnGone.
const (
	// ReasonExited means the process exited on its own (crash, harness
	// exit) — in-flight work must fail durably.
	ReasonExited = "runtime process exited"
	// ReasonStopped means Relay stopped the runtime deliberately (idle
	// sleep, session stop, daemon shutdown).
	ReasonStopped = "runtime stopped"
)

// SetState overrides a runtime's state (warm/active/stopping).
func (s *Supervisor) SetState(key, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.runtimes[key]; ok {
		rt.State = state
	}
}

// Get returns the runtime registered for key.
func (s *Supervisor) Get(key string) (*Runtime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[key]
	return rt, ok
}

// RuntimeForSession returns the runtime a session is bound to.
func (s *Supervisor) RuntimeForSession(sessionID string) (*Runtime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return nil, false
	}
	rt, ok := s.runtimes[key]
	return rt, ok
}

// View projects the runtime state for one session. ok is false when the
// session has no runtime (COLD).
func (s *Supervisor) View(sessionID string) (View, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return View{}, false
	}
	rt, ok := s.runtimes[key]
	if !ok {
		return View{}, false
	}
	act := rt.activity[sessionID]
	if act == "" {
		act = ActivityIdle
	}
	return View{
		RuntimeID: rt.ID,
		Kind:      rt.Kind,
		State:     rt.State,
		PID:       rt.PID,
		StartedAt: rt.StartedAt,
		Activity:  act,
	}, true
}
