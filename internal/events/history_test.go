package events

import (
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"testing"
)

// TestHistorySnapshotIsAtomicWithTransient: a snapshot's throughSeq never
// predates already-observed transient sequence space.
func TestHistorySnapshotIsAtomicWithTransient(t *testing.T) {
	_, _, _, s := mkStore(t, "atomic")
	if _, err := s.PublishDurable("message.user", payload(t, "a")); err != nil {
		t.Fatal(err)
	}
	tr, err := s.PublishTransient("message.agent.delta", payload(t, "chunk"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.HistorySnapshot(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq < tr.Seq {
		t.Fatalf("throughSeq %d predates transient seq %d", page.ThroughSeq, tr.Seq)
	}
	if page.ThroughSeq != s.Cursor() {
		t.Fatalf("throughSeq %d != cursor %d", page.ThroughSeq, s.Cursor())
	}
	// The snapshot cursor is always a valid subscription point.
	if _, err := s.Subscribe(page.ThroughSeq); err != nil {
		t.Fatalf("subscribe after snapshot: %v", err)
	}
}

// TestHistorySnapshotBounds: record count and byte budget both bound the
// page; hasMoreBefore reports the truncation.
func TestHistorySnapshotBounds(t *testing.T) {
	_, _, _, s := mkStore(t, "bounds")
	for i := 0; i < 10; i++ {
		if _, err := s.PublishDurable("t", payload(t, "0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.HistorySnapshot(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || !page.HasMoreBefore {
		t.Fatalf("count bound: %d records, hasMore=%v", len(page.Records), page.HasMoreBefore)
	}
	if page.Records[2].Seq != 10 {
		t.Fatalf("tail selection must be the newest records: %+v", page.Records)
	}
	// Byte budget: allow ~2 records worth of source bytes.
	one := int64(len(mustMarshal(t, page.Records[2])) + 1)
	page, err = s.HistorySnapshot(10, 2*one)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) > 2 || !page.HasMoreBefore {
		t.Fatalf("byte bound: %d records, hasMore=%v", len(page.Records), page.HasMoreBefore)
	}
	// Everything fits: no more-before.
	page, err = s.HistorySnapshot(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 10 || page.HasMoreBefore {
		t.Fatalf("full page: %d records, hasMore=%v", len(page.Records), page.HasMoreBefore)
	}
}

// TestEventFrameBound: an event whose canonical NDJSON frame exceeds the
// shared bound is refused before storage or exposure; one just below the
// bound publishes normally.
func TestEventFrameBound(t *testing.T) {
	_, _, _, s := mkStore(t, "frame")
	// Overhead of the envelope is small; size the payloads relative to
	// the bound.
	big := payloadOfSize(t, api.MaxEventFrameBytes)
	if _, err := s.PublishTransient("t", big); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversized transient: want ErrEventTooLarge, got %v", err)
	}
	if _, err := s.PublishDurable("t", big); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversized durable: want ErrEventTooLarge, got %v", err)
	}
	// Nothing was stored or retained.
	page, err := s.HistorySnapshot(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 {
		t.Fatalf("oversized durable event was stored: %+v", page.Records)
	}
	// A payload sized to fit does publish.
	ok := payloadOfSize(t, api.MaxEventFrameBytes-1024)
	if _, err := s.PublishTransient("t", ok); err != nil {
		t.Fatalf("payload just below the bound: %v", err)
	}
}
