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
// Cursor semantics: the canonical event cursor is the highest seq that has
// been consumed in this generation, and after a restart it is the
// persisted SeqHighWatermark. The cursor — not the last durable record —
// is what history snapshots hand to clients for the live cutover.
//
// Replay semantics: an explicit replay floor tracks what the bounded ring
// can no longer prove. Exact replay is possible only for
// replayFloor <= after <= cursor; below the floor the client must
// rehydrate durable history, above the cursor the request is rejected.
//
// Durability contract: a durable publish appends the canonical transcript
// record BEFORE exposing the event to any subscriber. A transient event is
// live-only and may be missed by a reconnecting client once it falls out
// of the bounded replay ring — durable completed transcript state is what
// lets clients converge.
//
// Resource contract: COLD sessions are cheap. Event state holds no
// transcript file handles — every operation opens the canonical
// transcript, works, and closes — and the replay ring is allocated
// lazily on first publish. A restored idle session costs bookkeeping
// only.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
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
	// ErrCursorTooOld means the requested after=N cursor is below the
	// replay floor: the bounded ring overwrote it, or it belongs to a
	// previous daemon generation. The client must rehydrate durable
	// history instead of silently receiving an incomplete stream.
	ErrCursorTooOld = errors.New("cursor too old")
	// ErrCursorAhead means the requested after=N cursor is above the
	// current canonical event cursor. A cursor from the future is never
	// established as a subscription.
	ErrCursorAhead = errors.New("cursor ahead of current event cursor")
	// ErrEventTooLarge means the canonical NDJSON frame of an event would
	// exceed the shared frame bound; the event is refused before it is
	// stored or exposed.
	ErrEventTooLarge = errors.New("event exceeds canonical frame bound")
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

// HistoryPage is one atomic history snapshot: durable transcript records
// plus the canonical event cursor at the same instant. ThroughSeq is the
// exact cursor a client subscribes after for a gap-free cutover.
type HistoryPage struct {
	Records       []store.Record
	ThroughSeq    uint64
	HasMoreBefore bool
}

// Session is the per-session event state: allocator + lazy replay ring +
// subscribers. It holds NO open file handles: transcript operations open
// the canonical transcript, work, and close, so a COLD session costs
// bookkeeping only.
type Session struct {
	mu     sync.Mutex
	id     string
	rs     *session.RelaySession
	metaMu *sync.Mutex
	ss     *store.Sessions

	next   uint64 // next seq to allocate
	limit  uint64 // durable reservation boundary (== rs.SeqHighWatermark)
	cursor uint64 // canonical event cursor: highest consumed seq this generation

	// floor is the explicit replay floor: a subscriber with
	// after < floor cannot be guaranteed exact replay. It starts at the
	// persisted watermark (previous generations' seq space) and advances
	// when the ring overwrites an event.
	floor uint64

	ring  []Event // nil until the first event needs retention
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

// Ensure creates (or returns) the event state for a managed session. The
// canonical transcript is opened once for O(1) validation (index
// alignment, no durable record above the reserved watermark) and closed
// immediately — no handles are retained.
//
// Cursor initialization: allocation resumes strictly AFTER the persisted
// watermark, and the current cursor IS the persisted watermark. That is
// exact even when the last durable record is far older: transient events
// and reserved blocks consumed that sequence space in previous
// generations and can never be replayed.
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
	last := tr.LastSeq()
	_ = tr.Close()
	// Read the durable watermark under MetaMu: it is the same lock the
	// seq-block reservation uses, so a concurrent reservation can never
	// hand out a seq below the value initialized here.
	watermark := m.Snapshot().SeqHighWatermark
	if last > watermark {
		return nil, fmt.Errorf(
			"events: session %s transcript seq %d exceeds durable watermark %d",
			id, last, watermark)
	}
	s := &Session{
		id:     id,
		rs:     m.Session,
		metaMu: &m.MetaMu,
		ss:     b.ss,
		next:   watermark + 1,
		limit:  watermark,
		cursor: watermark,
		floor:  watermark,
		subs:   map[*Subscription]struct{}{},
	}
	b.sessions[id] = s
	return s, nil
}

// Sessions returns a snapshot of every session's event state (read-only
// view; used by structural resource tests).
func (b *Broker) Sessions() []*Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s)
	}
	return out
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
		Seq: seq, SessionID: s.id, Type: typ,
		At: time.Now().UTC(), Payload: payload, Durable: true,
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
		Seq: seq, SessionID: s.id, Type: typ,
		At: time.Now().UTC(), Payload: payload, Durable: false,
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

// Subscribe registers a live subscriber atomically with the replay
// prefix: under the session lock it captures replayable events with
// Seq > afterSeq and registers the queue, so no published event can slip
// between replay capture and subscription.
//
// Exactness is decided by the explicit replay floor, not by ring
// arithmetic: exact replay requires replayFloor <= afterSeq <= cursor.
// Below the floor the ring (or the previous generation) may have dropped
// events; above the cursor the request is from the future. Both are
// stable errors, never a silently incomplete stream.
func (s *Session) Subscribe(afterSeq uint64) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if afterSeq > s.cursor {
		return nil, ErrCursorAhead
	}
	if afterSeq < s.floor {
		return nil, ErrCursorTooOld
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
	page := HistoryPage{ThroughSeq: s.cursor, Records: []store.Record{}}
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

// SubscriberCount returns the number of live subscribers (test seam).
func (s *Session) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// close ends all subscribers. Durable state and files are untouched; no
// file handles are held between operations.
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
	s.mu.Unlock()
	for _, sub := range subs {
		sub.end(reason)
	}
}
