package runtime

import (
	"testing"
	"time"
)

func TestSnapshotEmptyAndImmutable(t *testing.T) {
	s := New()
	if got := s.Snapshot(); len(got) != 0 {
		t.Fatalf("empty snapshot = %v", got)
	}
	// Mutating a returned snapshot must not reach supervisor state.
	now := time.Now()
	s.mu.Lock()
	rt := &Runtime{
		ID:        "rt1",
		Key:       "k1",
		Kind:      "fixture",
		PID:       4242,
		StartedAt: now,
		State:     "warm",
		sessions:  map[string]struct{}{"sess1": {}},
		activity:  map[string]Activity{"sess1": ActivityActive},
		mutations: map[string]struct{}{"sess1": {}},
		h:         newFakeProc(4242),
		ready:     make(chan struct{}),
	}
	rt.readyDone = true
	s.runtimes["k1"] = rt
	s.bySession["sess1"] = "k1"
	s.mu.Unlock()

	snaps := s.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d", len(snaps))
	}
	sn := snaps[0]
	if sn.ID != "rt1" || sn.PID != 4242 || sn.Kind != "fixture" || sn.State != "warm" {
		t.Fatalf("snapshot fields: %+v", sn)
	}
	if len(sn.Sessions) != 1 || sn.Sessions[0].SessionID != "sess1" ||
		sn.Sessions[0].Activity != ActivityActive || !sn.Sessions[0].Mutating {
		t.Fatalf("binding: %+v", sn.Sessions)
	}
	// Mutate the copy — supervisor internals unchanged.
	sn.Sessions[0].Activity = ActivityIdle
	sn.ID = "corrupt"
	if rt.activity["sess1"] != ActivityActive || rt.ID != "rt1" {
		t.Fatal("snapshot shares mutable state with the supervisor")
	}
}

func TestSnapshotOrderingAndValidate(t *testing.T) {
	s := New()
	s.mu.Lock()
	for _, k := range []string{"z/last", "a/first", "m/mid"} {
		s.runtimes[k] = &Runtime{
			ID:        "id-" + k,
			Key:       k,
			Kind:      "codex",
			PID:       1,
			StartedAt: time.Now(),
			State:     "warm",
			sessions:  map[string]struct{}{},
			activity:  map[string]Activity{},
			ready:     make(chan struct{}),
			readyDone: true,
			h:         newFakeProc(1),
		}
	}
	s.mu.Unlock()

	snaps := s.Snapshot()
	if len(snaps) != 3 || snaps[0].Key != "a/first" || snaps[2].Key != "z/last" {
		t.Fatalf("ordering: %v", snaps)
	}
	if !s.ValidateGeneration("m/mid", "id-m/mid", 1) {
		t.Fatal("ValidateGeneration must accept the same generation")
	}
	if s.ValidateGeneration("m/mid", "other-id", 1) ||
		s.ValidateGeneration("m/mid", "id-m/mid", 999) ||
		s.ValidateGeneration("absent", "x", 1) {
		t.Fatal("ValidateGeneration must fail closed on id/pid/key mismatch")
	}
	// Generation replacement: old snapshot must not revalidate.
	s.mu.Lock()
	s.runtimes["m/mid"] = &Runtime{
		ID:        "new-gen",
		Key:       "m/mid",
		PID:       2,
		sessions:  map[string]struct{}{},
		activity:  map[string]Activity{},
		ready:     make(chan struct{}),
		readyDone: true,
		h:         newFakeProc(2),
	}
	s.mu.Unlock()
	if s.ValidateGeneration("m/mid", "id-m/mid", 1) {
		t.Fatal("stale generation must not revalidate after replacement")
	}
}
