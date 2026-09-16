package events

import (
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"math"
	"testing"
	"time"
)

// TestReplayAndLiveOrdering: replay covers events after the cursor; live
// events follow with strictly increasing seq.
func TestReplayAndLiveOrdering(t *testing.T) {
	_, _, _, s := mkStore(t, "replay")
	for i := 0; i < 5; i++ {
		if _, err := s.PublishDurable("x", payload(t, fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	sub, err := s.Subscribe(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Replay) != 3 || sub.Replay[0].Seq != 3 {
		t.Fatalf("replay = %d events starting at %d", len(sub.Replay), sub.Replay[0].Seq)
	}
	// An event published after subscribe arrives live — no gap between
	// replay capture and registration.
	if _, err := s.PublishDurable("x", payload(t, "live")); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Seq != 6 {
			t.Fatalf("live seq = %d", ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("live event missing")
	}
}

// TestCursorTooOld: a cursor older than the bounded ring is refused
// explicitly instead of silently returning an incomplete stream.
func TestCursorTooOld(t *testing.T) {
	_, _, _, s := mkStore(t, "old")
	// Publish more than the ring can retain.
	for i := 0; i < ReplayRingSize+10; i++ {
		if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Subscribe(0); !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("want ErrCursorTooOld, got %v", err)
	}
	// A recent cursor still works exactly.
	if _, err := s.Subscribe(uint64(ReplayRingSize + 5)); err != nil {
		t.Fatalf("recent cursor: %v", err)
	}
}

// TestReplayRingBounded: the ring never retains more than its capacity.
func TestReplayRingBounded(t *testing.T) {
	_, _, _, s := mkStore(t, "ring")
	for i := 0; i < ReplayRingSize*2; i++ {
		if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	count := s.count
	s.mu.Unlock()
	if count != ReplayRingSize {
		t.Fatalf("ring count = %d, want %d", count, ReplayRingSize)
	}
}

// TestRestartCursorIsWatermark is the canonical restart regression:
// generation 1 publishes a durable event (seq 1) and a transient event
// (seq 2) — the transient consumes sequence space but is never persisted.
// After restart, history hydration must report throughSeq == the
// persisted watermark (NOT the last durable seq), subscribing after that
// cursor must work, and subscribing after the old durable seq must be
// refused: seq 2 can no longer be replayed.
func TestRestartCursorIsWatermark(t *testing.T) {
	ss, m, _, s := mkStore(t, "restart")
	first, err := s.PublishDurable("message.user", payload(t, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 {
		t.Fatalf("durable seq = %d", first.Seq)
	}
	transient, err := s.PublishTransient("message.agent.delta", payload(t, "chunk"))
	if err != nil {
		t.Fatal(err)
	}
	if transient.Seq != 2 {
		t.Fatalf("transient seq = %d", transient.Seq)
	}
	watermark := m.Session.SeqHighWatermark
	if watermark < 2 {
		t.Fatalf("watermark = %d, want >= 2", watermark)
	}

	// Restart: fresh broker + fresh session state from durable metadata.
	loaded, err := ss.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded[0].SeqHighWatermark; got != watermark {
		t.Fatalf("persisted watermark = %d, want %d", got, watermark)
	}
	m2 := &session.Managed{Session: loaded[0]}
	s2, err := NewBroker(ss).Ensure(m2)
	if err != nil {
		t.Fatal(err)
	}

	// History hydration: throughSeq is the persisted watermark.
	page, err := s2.HistorySnapshot(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != watermark {
		t.Fatalf("throughSeq = %d, want persisted watermark %d", page.ThroughSeq, watermark)
	}
	if len(page.Records) != 1 || page.Records[0].Seq != 1 {
		t.Fatalf("durable records = %+v", page.Records)
	}

	// Subscribing after the snapshot cursor is exact and receives future
	// events.
	sub, err := s2.Subscribe(page.ThroughSeq)
	if err != nil {
		t.Fatalf("subscribe after snapshot cursor: %v", err)
	}
	next, err := s2.PublishTransient("t", payload(t, "after"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq <= watermark {
		t.Fatalf("next seq %d must exceed watermark %d", next.Seq, watermark)
	}
	select {
	case ev := <-sub.Events():
		if ev.Seq != next.Seq {
			t.Fatalf("live seq = %d, want %d", ev.Seq, next.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("live event missing after restart subscribe")
	}

	// The old durable cursor can no longer be served exactly: transient
	// seq 2 is gone forever.
	if _, err := s2.Subscribe(1); !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("subscribe(1) after restart: want ErrCursorTooOld, got %v", err)
	}
	// Cursor ahead of the current event cursor is rejected.
	if _, err := s2.Subscribe(s2.Cursor() + 1); !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("subscribe(cursor+1): want ErrCursorAhead, got %v", err)
	}
	// No overflow: the maximum uint64 cursor is deterministically ahead.
	if _, err := s2.Subscribe(math.MaxUint64); !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("subscribe(MaxUint64): want ErrCursorAhead, got %v", err)
	}
}

// TestReplayFloorTracksOverwrites: the floor advances exactly when the
// ring overwrites an event, and never with +1 arithmetic.
func TestReplayFloorTracksOverwrites(t *testing.T) {
	_, _, _, s := mkStore(t, "floor")
	if got := s.ReplayFloor(); got != 0 {
		t.Fatalf("fresh floor = %d", got)
	}
	for i := 0; i < ReplayRingSize; i++ {
		if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.ReplayFloor(); got != 0 {
		t.Fatalf("floor advanced before any overwrite: %d", got)
	}
	// One more event overwrites seq 1.
	if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
		t.Fatal(err)
	}
	if got := s.ReplayFloor(); got != 1 {
		t.Fatalf("floor after first overwrite = %d, want 1", got)
	}
	// Subscribe(1) is still exact (the event at seq 1 is the overwritten
	// one — a client at cursor 1 has already seen it).
	if _, err := s.Subscribe(1); err != nil {
		t.Fatalf("subscribe(1): %v", err)
	}
	// Subscribe(0) is refused.
	if _, err := s.Subscribe(0); !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("subscribe(0): want ErrCursorTooOld, got %v", err)
	}
}

// TestLazyReplayRing: the ring is allocated on first publish, not on
// session construction.
func TestLazyReplayRing(t *testing.T) {
	_, _, _, s := mkStore(t, "lazy")
	if s.RingAllocated() {
		t.Fatal("ring allocated for an inactive session")
	}
	if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
		t.Fatal(err)
	}
	if !s.RingAllocated() {
		t.Fatal("ring not allocated on first publish")
	}
}

// ---------------------------------------------------------------------
// COLD session resource model
// ---------------------------------------------------------------------
