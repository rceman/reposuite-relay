package events

import (
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/store"
	"math"
	"time"
)

// allocLocked returns the next canonical seq, reserving a durable block
// when the current reservation is exhausted. Caller holds s.mu.
//
// Block reservation: persist watermark W+SeqBlockSize BEFORE handing out
// any seq in the new block, so a crash can only create gaps — never reuse.
// uint64 exhaustion fails closed.
func (s *Session) allocLocked() (uint64, error) {
	if s.closed {
		return 0, ErrClosed
	}
	if s.next > s.limit {
		if math.MaxUint64-SeqBlockSize < s.limit {
			return 0, fmt.Errorf("events: session %s sequence space exhausted", s.id)
		}
		newLimit := s.limit + SeqBlockSize
		old := s.rs.SeqHighWatermark
		s.metaMu.Lock()
		s.rs.SeqHighWatermark = newLimit
		s.rs.UpdatedAt = time.Now().UTC()
		err := s.ss.Save(s.rs)
		if err != nil {
			// Roll the in-memory field back: a too-high durable watermark
			// is harmless (gaps are legal), a stale in-memory one is not.
			s.rs.SeqHighWatermark = old
		}
		s.metaMu.Unlock()
		if err != nil {
			return 0, fmt.Errorf("events: reserve seq block for %s: %w", s.id, err)
		}
		s.limit = newLimit
	}
	seq := s.next
	s.next++
	s.cursor = seq
	return seq, nil
}

// PublishDurable allocates a seq, appends the canonical durable transcript
// record, and only then exposes the event to replay/live subscribers.
// A durable event is never visible before it is stored. The transcript is
// opened and closed around the append — no handle is retained.
func (s *Session) PublishDurable(typ string, payload json.RawMessage) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.allocLocked()
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Seq:       seq,
		SessionID: s.id,
		Type:      typ,
		At:        time.Now().UTC(),
		Payload:   payload,
		Durable:   true,
	}
	if err := checkFrame(ev); err != nil {
		return Event{}, err
	}
	rec := store.Record{
		Version: store.RecordVersion, Seq: seq, Type: typ,
		At: ev.At, Payload: payload,
	}
	tr, err := s.ss.Transcript(s.id)
	if err != nil {
		return Event{}, fmt.Errorf("events: durable publish %s seq %d: %w", s.id, seq, err)
	}
	appErr := tr.Append(rec)
	_ = tr.Close()
	if appErr != nil {
		// Never expose an unstored "durable" event. The consumed seq
		// becomes a legal gap; the next operation reopens the transcript
		// and repairs any torn/unindexed tail.
		return Event{}, fmt.Errorf("events: durable publish %s seq %d: %w", s.id, seq, appErr)
	}
	s.ringAddLocked(ev)
	s.broadcastLocked(ev)
	return ev, nil
}

// PublishTransient allocates a seq and exposes the event live — it is
// never written to the transcript.
func (s *Session) PublishTransient(typ string, payload json.RawMessage) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.allocLocked()
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Seq:       seq,
		SessionID: s.id,
		Type:      typ,
		At:        time.Now().UTC(),
		Payload:   payload,
		Durable:   false,
	}
	if err := checkFrame(ev); err != nil {
		return Event{}, err
	}
	s.ringAddLocked(ev)
	s.broadcastLocked(ev)
	return ev, nil
}

// checkFrame enforces the canonical NDJSON frame bound on publication:
// an event the canonical client could not decode is never stored or
// exposed.
func checkFrame(ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("events: marshal event: %w", err)
	}
	if len(line)+1 > api.MaxEventFrameBytes {
		return fmt.Errorf("%w: %d bytes > %d", ErrEventTooLarge, len(line)+1, api.MaxEventFrameBytes)
	}
	return nil
}

// ringAddLocked appends to the bounded replay ring, allocating the ring
// lazily on first use and advancing the replay floor past whatever the
// ring overwrites.
func (s *Session) ringAddLocked(ev Event) {
	if s.ring == nil {
		s.ring = make([]Event, ReplayRingSize)
	}
	if s.count == len(s.ring) {
		// About to overwrite the oldest retained event: seqs at or below
		// it can no longer be replayed exactly.
		if oldest := s.ring[s.head].Seq; oldest > s.floor {
			s.floor = oldest
		}
		s.ring[s.head] = ev
		s.head = (s.head + 1) % len(s.ring)
		return
	}
	s.ring[(s.head+s.count)%len(s.ring)] = ev
	s.count++
}

// broadcastLocked delivers to every subscriber; a subscriber whose queue
// is full is evicted deterministically and never blocks the producer.
func (s *Session) broadcastLocked(ev Event) {
	for sub := range s.subs {
		select {
		case sub.ch <- ev:
		default:
			delete(s.subs, sub)
			sub.end(ErrSubscriberEvicted)
		}
	}
}
