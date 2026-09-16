package runtime

import (
	"fmt"
)

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
