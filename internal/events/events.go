// Package events implements the canonical per-session event foundation:
// a monotonic sequence allocator with durable block reservation, a bounded
// recent-event replay ring, and bounded live subscribers.
//
// This is deliberately a small Relay-specific component, not a generic
// event bus. Events are the domain currency for HTTP NDJSON streaming, a
// future TUI, and a future Gateway bridge; they are not transport structs.
//
// Sequence semantics: every event (durable or transient) consumes a
// canonical per-session uint64 seq that strictly increases and is never
// reused. Transient events are not persisted, so the durable transcript
// lastSeq alone cannot guarantee non-reuse across restart — the durable
// SeqHighWatermark in session metadata reserves sequence space in blocks.
// Gaps (reserved-but-unused, or transient) are legal and expected.
//
// Durability contract: a durable publish appends the canonical transcript
// record BEFORE exposing the event to any subscriber. A transient event is
// live-only and may be missed by a reconnecting client once it falls out
// of the bounded replay ring — durable completed transcript state is what
// lets clients converge.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// Bounds. Explicit and modest — nothing here grows without limit.
const (
	// SeqBlockSize is the sequence space reserved per durable metadata
	// update. Larger blocks amortize fsyncs; unused numbers become gaps.
	SeqBlockSize = 1024
	// ReplayRingSize bounds recent-event retention per session.
	ReplayRingSize = 2048
	// SubscriberQueueSize bounds a subscriber's pending queue. A slow
	// subscriber is evicted, never allowed to block the producer.
	SubscriberQueueSize = 256
)

// Event is one canonical session event.
type Event struct {
	Seq       uint64          `json:"seq"`
	SessionID string          `json:"sessionId"`
	Type      string          `json:"type"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Durable   bool            `json:"durable"`
}

// Stable cursor/stream errors.
var (
	// ErrCursorTooOld means the requested after=N cursor is older than the
	// bounded replay ring can serve — the client must rehydrate durable
	// history instead of silently receiving an incomplete stream.
	ErrCursorTooOld = errors.New("cursor too old")
	// ErrClosed means the session's event state is gone (session deleted
	// or daemon shutting down).
	ErrClosed = errors.New("event session closed")
	// ErrSubscriberEvicted is the deterministic reason a slow subscriber's
	// stream is terminated.
	ErrSubscriberEvicted = errors.New("subscriber evicted: queue full")
)

// Subscription is one live subscriber: a replay prefix captured atomically
// at subscribe time plus a bounded live queue.
type Subscription struct {
	Replay []Event

	ch     chan Event
	mu     sync.Mutex
	reason error
}

// Events returns the live queue. The channel is closed when the
// subscription ends — check Err for the deterministic reason.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Err returns why the subscription ended (nil while still live, or
// ErrSubscriberEvicted / ErrClosed after the channel closes).
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Subscription) end(reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == nil {
		s.reason = reason
	}
	close(s.ch)
}

// Session is the per-session event state: allocator + replay ring +
// subscribers. It owns the session's open transcript handle.
type Session struct {
	mu     sync.Mutex
	id     string
	rs     *session.RelaySession
	metaMu *sync.Mutex
	ss     *store.Sessions
	tr     *store.Transcript

	next  uint64 // next seq to allocate
	limit uint64 // durable reservation boundary (== rs.SeqHighWatermark)

	ring  []Event
	head  int
	count int

	subs   map[*Subscription]struct{}
	closed bool
}

// Broker manages event state for every live session in the daemon.
type Broker struct {
	mu       sync.Mutex
	ss       *store.Sessions
	sessions map[string]*Session // by RelaySession ID
}

// NewBroker returns an empty broker backed by the durable store.
func NewBroker(ss *store.Sessions) *Broker {
	return &Broker{ss: ss, sessions: map[string]*Session{}}
}

// Ensure creates (or returns) the event state for a managed session:
// opens the transcript via the healthy O(1) path and validates that no
// durable record exceeds the reserved sequence watermark. Restart resumes
// allocation strictly AFTER the persisted watermark so no seq is reused.
func (b *Broker) Ensure(m *session.Managed) (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := m.Session.ID
	if s, ok := b.sessions[id]; ok {
		return s, nil
	}
	tr, err := b.ss.Transcript(id)
	if err != nil {
		return nil, fmt.Errorf("events: open transcript %s: %w", id, err)
	}
	if last := tr.LastSeq(); last > m.Session.SeqHighWatermark {
		tr.Close()
		return nil, fmt.Errorf(
			"events: session %s transcript seq %d exceeds durable watermark %d",
			id, last, m.Session.SeqHighWatermark)
	}
	s := &Session{
		id:     id,
		rs:     m.Session,
		metaMu: &m.MetaMu,
		ss:     b.ss,
		tr:     tr,
		next:   m.Session.SeqHighWatermark + 1,
		limit:  m.Session.SeqHighWatermark,
		ring:   make([]Event, ReplayRingSize),
		subs:   map[*Subscription]struct{}{},
	}
	b.sessions[id] = s
	return s, nil
}

// Get returns the event state for a session ID.
func (b *Broker) Get(id string) (*Session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[id]
	return s, ok
}

// Remove tears down a session's event state: subscribers end with
// ErrClosed and the transcript handle closes. Durable files are untouched.
func (b *Broker) Remove(id string) {
	b.mu.Lock()
	s, ok := b.sessions[id]
	delete(b.sessions, id)
	b.mu.Unlock()
	if ok {
		s.close(ErrClosed)
	}
}

// CloseAll tears down every session's event state (daemon shutdown):
// all subscribers are disconnected, durable transcripts are not deleted.
func (b *Broker) CloseAll() {
	b.mu.Lock()
	all := make([]*Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		all = append(all, s)
	}
	b.sessions = map[string]*Session{}
	b.mu.Unlock()
	for _, s := range all {
		s.close(ErrClosed)
	}
}

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
	return seq, nil
}

// PublishDurable allocates a seq, appends the canonical durable transcript
// record, and only then exposes the event to replay/live subscribers.
// A durable event is never visible before it is stored.
func (s *Session) PublishDurable(typ string, payload json.RawMessage) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.allocLocked()
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Seq: seq, SessionID: s.id, Type: typ,
		At: time.Now().UTC(), Payload: payload, Durable: true,
	}
	rec := store.Record{
		Version: store.RecordVersion, Seq: seq, Type: typ,
		At: ev.At, Payload: payload,
	}
	if err := s.tr.Append(rec); err != nil {
		// Never expose an unstored "durable" event. The consumed seq
		// becomes a legal gap.
		return Event{}, fmt.Errorf("events: durable publish %s seq %d: %w", s.id, seq, err)
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
		Seq: seq, SessionID: s.id, Type: typ,
		At: time.Now().UTC(), Payload: payload, Durable: false,
	}
	s.ringAddLocked(ev)
	s.broadcastLocked(ev)
	return ev, nil
}

// ringAddLocked appends to the bounded replay ring (overwriting oldest).
func (s *Session) ringAddLocked(ev Event) {
	if len(s.ring) == 0 {
		return
	}
	s.ring[(s.head+s.count)%len(s.ring)] = ev
	if s.count < len(s.ring) {
		s.count++
	} else {
		s.head = (s.head + 1) % len(s.ring)
	}
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

// Subscribe registers a live subscriber atomically with the replay
// prefix: under the session lock it captures replayable events with
// Seq > afterSeq and registers the queue, so no published event can slip
// between replay capture and subscription. If the cursor is older than
// the bounded ring can serve, ErrCursorTooOld is returned instead of a
// silently incomplete stream.
func (s *Session) Subscribe(afterSeq uint64) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	// Ring coverage: the oldest retained seq must be <= afterSeq+1 for an
	// exact no-gap replay (or the ring may be empty only if nothing was
	// ever published beyond afterSeq).
	// count == 0 means nothing was ever published — any cursor is exact.
	if s.count > 0 {
		oldest := s.ring[s.head].Seq
		if afterSeq+1 < oldest {
			return nil, ErrCursorTooOld
		}
	}
	sub := &Subscription{ch: make(chan Event, SubscriberQueueSize)}
	for i := 0; i < s.count; i++ {
		ev := s.ring[(s.head+i)%len(s.ring)]
		if ev.Seq > afterSeq {
			sub.Replay = append(sub.Replay, ev)
		}
	}
	s.subs[sub] = struct{}{}
	return sub, nil
}

// Unsubscribe ends a subscription (client disconnect).
func (s *Session) Unsubscribe(sub *Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		sub.end(nil)
	}
}

// ID returns the RelaySession ID this event state belongs to.
func (s *Session) ID() string { return s.id }

// Tail returns the latest up-to-limit durable transcript records plus
// throughSeq (delegates to the session's open transcript).
func (s *Session) Tail(limit int) ([]store.Record, uint64, error) {
	return s.tr.Tail(limit)
}

// LastSeq returns the highest seq allocated so far (0 = none).
func (s *Session) LastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next - 1
}

// SubscriberCount returns the number of live subscribers (test seam).
func (s *Session) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// close ends all subscribers and releases the transcript handle.
func (s *Session) close(reason error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subs = map[*Subscription]struct{}{}
	tr := s.tr
	s.mu.Unlock()
	for _, sub := range subs {
		sub.end(reason)
	}
	if tr != nil {
		_ = tr.Close()
	}
}
