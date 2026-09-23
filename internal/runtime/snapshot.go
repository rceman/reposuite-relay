package runtime

import (
	"sort"
	"time"
)

// BoundSession is one immutable session binding inside a snapshot.
// Activity and Mutating reflect Relay-observed native state only — they
// say nothing about external child processes or background work.
type BoundSession struct {
	SessionID string
	Activity  Activity
	Mutating  bool
}

// RuntimeSnapshot is an immutable projection of one runtime generation,
// copied under Supervisor.mu. No mutable internal maps escape: every
// slice is freshly allocated. Ordering is deterministic (by Key).
type RuntimeSnapshot struct {
	ID        string // ephemeral runtime identity
	Key       string
	Kind      string
	Shared    bool
	PID       int
	StartedAt time.Time
	State     string
	Sessions  []BoundSession
}

// Snapshot returns an immutable projection of every live runtime
// generation, sorted by runtime key. Callers may read/process the
// result without holding the supervisor lock — expensive work such as
// /proc sampling must happen AFTER the snapshot is released.
func (s *Supervisor) Snapshot() []RuntimeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.runtimes))
	for k := range s.runtimes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]RuntimeSnapshot, 0, len(keys))
	for _, k := range keys {
		rt := s.runtimes[k]
		bindings := make([]BoundSession, 0, len(rt.sessions))
		for id := range rt.sessions {
			act := rt.activity[id]
			if act == "" {
				act = ActivityIdle
			}
			bindings = append(bindings, BoundSession{
				SessionID: id,
				Activity:  act,
				Mutating:  rt.mutations != nil && has(rt.mutations, id),
			})
		}
		sort.Slice(bindings, func(i, j int) bool {
			return bindings[i].SessionID < bindings[j].SessionID
		})
		out = append(out, RuntimeSnapshot{
			ID:        rt.ID,
			Key:       rt.Key,
			Kind:      rt.Kind,
			Shared:    rt.Shared,
			PID:       rt.PID,
			StartedAt: rt.StartedAt,
			State:     rt.State,
			Sessions:  bindings,
		})
	}
	return out
}

// ValidateGeneration reports whether runtime key still maps to the same
// generation (runtime ID AND PID) observed in an earlier snapshot.
// Used to revalidate after out-of-lock work: a generation that was
// replaced mid-sample must fail closed so stale resources are never
// attributed to a new runtime.
func (s *Supervisor) ValidateGeneration(key, id string, pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[key]
	return ok && rt.ID == id && rt.PID == pid
}

func has(set map[string]struct{}, id string) bool {
	_, ok := set[id]
	return ok
}
