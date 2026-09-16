package events

import (
	"errors"
	"testing"
	"time"
)

// TestSlowSubscriberEvicted: a full queue evicts that subscriber
// deterministically; fast subscribers continue and the producer never
// blocks.
func TestSlowSubscriberEvicted(t *testing.T) {
	_, _, _, s := mkStore(t, "slow")
	slow, err := s.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := s.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	// Drain `fast` in lockstep so its queue never fills; never drain
	// `slow`. Draining concurrently is not enough: under load the fast
	// subscriber can fall behind and be evicted like the slow one.
	for i := 0; i < SubscriberQueueSize+50; i++ {
		if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fast.Events():
		case <-time.After(2 * time.Second):
			t.Fatal("fast subscriber received no event")
		}
	}
	// Slow was evicted with a deterministic reason.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(slow.Err(), ErrSubscriberEvicted) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !errors.Is(slow.Err(), ErrSubscriberEvicted) {
		t.Fatalf("slow subscriber not evicted: %v", slow.Err())
	}
	// Fast is still live.
	if s.SubscriberCount() != 1 {
		t.Fatalf("subscriber count = %d, want 1", s.SubscriberCount())
	}
	s.Unsubscribe(fast)
}

// TestRemoveClosesSubscribers: session teardown ends subscribers with
// ErrClosed.
func TestRemoveClosesSubscribers(t *testing.T) {
	_, m, b, s := mkStore(t, "close")
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	b.Remove(m.Session.ID)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(sub.Err(), ErrClosed) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber not closed: %v", sub.Err())
}

// TestCloseAllClosesSubscribers: daemon shutdown disconnects everyone.
func TestCloseAllClosesSubscribers(t *testing.T) {
	_, _, b, s := mkStore(t, "closeall")
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	b.CloseAll()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(sub.Err(), ErrClosed) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber not closed: %v", sub.Err())
}
