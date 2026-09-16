package events

import (
	"fmt"
	"github.com/rceman/reposuite-relay/internal/store"
)

// HistorySnapshot atomically captures durable transcript records plus the
// canonical event cursor at the same instant. It is serialized against
// every publish, so records and ThroughSeq belong to one coherent
// boundary: a client that hydrates this page and subscribes after
// ThroughSeq sees every subsequent event exactly once.
//
// ThroughSeq is the event cursor — NOT the last durable record seq. After
// a restart the cursor is the persisted watermark even when the newest
// durable record is far older; the gap is legal and permanent.
//
// The transcript is opened for the read and closed immediately.
func (s *Session) HistorySnapshot(limit int, maxBytes int64) (HistoryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return HistoryPage{}, ErrClosed
	}
	page := HistoryPage{
		ThroughSeq: s.cursor,
		Records:    []store.Record{},
	}
	if limit <= 0 {
		return page, nil
	}
	tr, err := s.ss.Transcript(s.id)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("events: history snapshot %s: %w", s.id, err)
	}
	recs, _, hasMore, err := tr.TailBounded(limit, maxBytes)
	_ = tr.Close()
	if err != nil {
		return HistoryPage{}, fmt.Errorf("events: history snapshot %s: %w", s.id, err)
	}
	if recs != nil {
		page.Records = recs
	}
	page.HasMoreBefore = hasMore
	return page, nil
}

// Cursor returns the canonical event cursor: the highest seq consumed in
// this generation (== the persisted watermark after a restart).
func (s *Session) Cursor() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

// ReplayFloor returns the explicit replay floor (test seam): a subscriber
// with after < floor cannot be guaranteed exact replay.
func (s *Session) ReplayFloor() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.floor
}

// RingAllocated reports whether the replay ring backing storage exists
// (test seam): false for a session that never published.
func (s *Session) RingAllocated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ring != nil
}
