package runtime

import (
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
)

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

// StopIfUnbound atomically stops a generation that has ZERO bound
// sessions — the orphan-reap path for a just-spawned generation whose
// only attach failed. Unlike StopIfIdle (a general sleep primitive for
// idle-but-bound generations) this declines whenever a session is
// bound: it must never destroy another session's runtime. The caller
// serializes attaches so no pre-bind work can be in flight; the bound
// count itself is checked and the generation removed under the same
// lock, so the decision is atomic. Returns (true, nil) when reaped.
func (s *Supervisor) StopIfUnbound(key string) (bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false, ErrClosed
	}
	rt, ok := s.runtimes[key]
	if !ok {
		s.mu.Unlock()
		return false, ErrRuntimeGone
	}
	if len(rt.sessions) > 0 {
		s.mu.Unlock()
		return false, nil // shared generation in use — decline
	}
	rt.State = session.RuntimeStopping
	delete(s.runtimes, key) // definite stop: the watcher must not re-handle it
	s.mu.Unlock()
	return true, s.finishStop(rt)
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
// reports the runtime gone exactly once. A runtime whose process is still
// spawning is abandoned instead: the spawn owner stops the process and
// never registers it.
func (s *Supervisor) finishStop(rt *Runtime) error {
	s.mu.Lock()
	rt.abandoned = true
	rt.resolveReadyLocked()
	for id := range rt.sessions {
		if s.bySession[id] == rt.Key {
			delete(s.bySession, id)
		}
	}
	rt.sessions = map[string]struct{}{}
	rt.activity = map[string]Activity{}
	rt.mutations = map[string]struct{}{}
	h := rt.h
	onGone := s.OnGone
	s.mu.Unlock()
	var err error
	if h != nil {
		err = h.Stop()
	}
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
