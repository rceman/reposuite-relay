package runtime

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
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

	h Harness

	sessions  map[string]struct{} // bound RelaySession IDs
	activity  map[string]Activity
	mutations map[string]struct{} // in-flight native mutations (cancel)
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

// Ensure returns the runtime for key, spawning one via spawn when absent.
// newID supplies the ephemeral runtime identity. The returned runtime is
// in RuntimeStarting state: it becomes usable only after the caller
// completes its handshake and calls MarkReady (or FailStart on failure).
// Concurrent Ensure calls for the same key converge on exactly one spawn.
func (s *Supervisor) Ensure(key, kind, newID string, shared bool, spawn SpawnFunc) (*Runtime, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if rt, ok := s.runtimes[key]; ok {
		s.mu.Unlock()
		return rt, nil
	}
	s.mu.Unlock()

	// Spawn outside the lock (process startup can block), then converge:
	// the first runtime to register wins, losers stop their process.
	h, err := spawn()
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		ID:        newID,
		Key:       key,
		Kind:      kind,
		Shared:    shared,
		PID:       h.PID(),
		StartedAt: time.Now(),
		State:     session.RuntimeStarting,
		h:         h,
		sessions:  map[string]struct{}{},
		activity:  map[string]Activity{},
		mutations: map[string]struct{}{},
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = h.Stop()
		return nil, ErrClosed
	}
	if existing, ok := s.runtimes[key]; ok {
		s.mu.Unlock()
		_ = h.Stop() // lost the race — never leak the extra process
		return existing, nil
	}
	s.runtimes[key] = rt
	s.mu.Unlock()
	go s.watch(rt)
	return rt, nil
}

// watch observes the runtime process and reacts to an unexpected exit.
func (s *Supervisor) watch(rt *Runtime) {
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

// Reasons reported to OnGone.
const (
	// ReasonExited means the process exited on its own (crash, harness
	// exit) — in-flight work must fail durably.
	ReasonExited = "runtime process exited"
	// ReasonStopped means Relay stopped the runtime deliberately (idle
	// sleep, session stop, daemon shutdown).
	ReasonStopped = "runtime stopped"
)

// MarkReady promotes a runtime from starting to warm after a successful
// initialization handshake. Spawned is not ready.
func (s *Supervisor) MarkReady(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[key]
	if !ok {
		return ErrRuntimeGone
	}
	if rt.State == session.RuntimeStarting {
		rt.State = session.RuntimeWarm
	}
	return nil
}

// SetState overrides a runtime's state (warm/active/stopping).
func (s *Supervisor) SetState(key, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.runtimes[key]; ok {
		rt.State = state
	}
}

// FailStart stops a runtime whose handshake failed: the process tree is
// reaped and no runtime remains registered.
func (s *Supervisor) FailStart(key string) error {
	return s.stopKey(key, false)
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

// Bind attaches a session to a runtime (after its native thread has been
// started or resumed in that generation). Re-binding to a different
// runtime moves the session.
func (s *Supervisor) Bind(key, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[key]
	if !ok {
		return ErrRuntimeGone
	}
	if prev, ok := s.bySession[sessionID]; ok && prev != key {
		if prevRT, ok := s.runtimes[prev]; ok {
			delete(prevRT.sessions, sessionID)
			delete(prevRT.activity, sessionID)
			delete(prevRT.mutations, sessionID)
		}
	}
	s.bySession[sessionID] = key
	rt.sessions[sessionID] = struct{}{}
	return nil
}

// Unbind detaches a session from its runtime (session delete, or runtime
// death cleanup). The runtime keeps running.
func (s *Supervisor) Unbind(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return
	}
	delete(s.bySession, sessionID)
	if rt, ok := s.runtimes[key]; ok {
		delete(rt.sessions, sessionID)
		delete(rt.activity, sessionID)
		delete(rt.mutations, sessionID)
	}
}

// SetActivity records the per-session activity state.
func (s *Supervisor) SetActivity(sessionID string, a Activity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return
	}
	if rt, ok := s.runtimes[key]; ok {
		rt.activity[sessionID] = a
	}
}

// Activity returns the per-session activity (idle when unknown).
func (s *Supervisor) Activity(sessionID string) Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return ActivityIdle
	}
	rt, ok := s.runtimes[key]
	if !ok {
		return ActivityIdle
	}
	if a := rt.activity[sessionID]; a != "" {
		return a
	}
	return ActivityIdle
}

// BeginMutation marks an in-progress native mutation (cancel, native
// session mutation) for a session — a sleep blocker that also serializes
// overlapping mutations.
func (s *Supervisor) BeginMutation(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return ErrRuntimeGone
	}
	rt, ok := s.runtimes[key]
	if !ok {
		return ErrRuntimeGone
	}
	if _, busy := rt.mutations[sessionID]; busy {
		return fmt.Errorf("%w: mutation already in progress", ErrRuntimeBusy)
	}
	rt.mutations[sessionID] = struct{}{}
	return nil
}

// EndMutation clears a mutation blocker.
func (s *Supervisor) EndMutation(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.bySession[sessionID]
	if !ok {
		return
	}
	if rt, ok := s.runtimes[key]; ok {
		delete(rt.mutations, sessionID)
	}
}

// CanSleep reports whether a runtime may be stopped, plus the blocking
// reason. Sleep blockers: any active turn, any pending requested input,
// any in-progress cancel/native mutation.
func (s *Supervisor) CanSleep(key string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[key]
	if !ok {
		return false, "runtime not present"
	}
	for id := range rt.sessions {
		switch rt.activity[id] {
		case ActivityActive:
			return false, "active turn in session " + id
		case ActivityWaitingInput:
			return false, "pending input in session " + id
		}
		if _, mutating := rt.mutations[id]; mutating {
			return false, "in-progress mutation in session " + id
		}
	}
	return true, ""
}

// StopIfIdle stops a runtime when no sleep blocker exists. Returns
// ErrRuntimeBusy (with the reason) otherwise.
func (s *Supervisor) StopIfIdle(key string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	rt, ok := s.runtimes[key]
	if !ok {
		s.mu.Unlock()
		return ErrRuntimeGone
	}
	for id := range rt.sessions {
		switch rt.activity[id] {
		case ActivityActive:
			s.mu.Unlock()
			return fmt.Errorf("%w: active turn in session %s", ErrRuntimeBusy, id)
		case ActivityWaitingInput:
			s.mu.Unlock()
			return fmt.Errorf("%w: pending input in session %s", ErrRuntimeBusy, id)
		}
		if _, mutating := rt.mutations[id]; mutating {
			s.mu.Unlock()
			return fmt.Errorf("%w: in-progress mutation in session %s", ErrRuntimeBusy, id)
		}
	}
	rt.State = session.RuntimeStopping
	delete(s.runtimes, key) // definite stop: the watcher must not re-handle it
	s.mu.Unlock()
	return s.finishStop(rt)
}

// Stop stops a runtime unconditionally (bounded, whole tree, confirmed).
func (s *Supervisor) Stop(key string) error {
	return s.stopKey(key, true)
}

func (s *Supervisor) stopKey(key string, markStopping bool) error {
	s.mu.Lock()
	rt, ok := s.runtimes[key]
	if !ok {
		s.mu.Unlock()
		return ErrRuntimeGone
	}
	if markStopping {
		rt.State = session.RuntimeStopping
	}
	delete(s.runtimes, key)
	s.mu.Unlock()
	return s.finishStop(rt)
}

// finishStop stops the process tree, detaches every bound session, and
// reports the runtime gone exactly once.
func (s *Supervisor) finishStop(rt *Runtime) error {
	err := rt.h.Stop()
	s.mu.Lock()
	for id := range rt.sessions {
		if s.bySession[id] == rt.Key {
			delete(s.bySession, id)
		}
	}
	rt.sessions = map[string]struct{}{}
	rt.activity = map[string]Activity{}
	rt.mutations = map[string]struct{}{}
	onGone := s.OnGone
	s.mu.Unlock()
	if onGone != nil {
		onGone(rt.Key, rt, ReasonStopped)
	}
	return err
}

// StopSession handles session deletion: a dedicated runtime (fixture,
// one runtime per session) is stopped and reaped; a shared runtime
// (Codex) is only detached — other sessions keep using it.
func (s *Supervisor) StopSession(sessionID string) error {
	s.mu.Lock()
	key, ok := s.bySession[sessionID]
	if !ok {
		s.mu.Unlock()
		return nil // COLD session — nothing to stop
	}
	rt, ok := s.runtimes[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	shared := rt.Shared
	if shared {
		delete(s.bySession, sessionID)
		delete(rt.sessions, sessionID)
		delete(rt.activity, sessionID)
		delete(rt.mutations, sessionID)
		s.mu.Unlock()
		return nil
	}
	// Dedicated: the runtime exists only for this session.
	rt.State = session.RuntimeStopping
	delete(s.runtimes, key)
	s.mu.Unlock()
	return s.finishStop(rt)
}

// StopAll stops every runtime (daemon shutdown) and returns the first
// stop error, if any. All processes are reaped regardless.
func (s *Supervisor) StopAll() error {
	s.mu.Lock()
	s.closed = true
	all := make([]*Runtime, 0, len(s.runtimes))
	for key, rt := range s.runtimes {
		rt.State = session.RuntimeStopping
		all = append(all, rt)
		delete(s.runtimes, key)
	}
	s.bySession = map[string]string{}
	s.mu.Unlock()
	var first error
	for _, rt := range all {
		if err := s.finishStop(rt); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Len returns the number of live runtimes (test seam).
func (s *Supervisor) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runtimes)
}
