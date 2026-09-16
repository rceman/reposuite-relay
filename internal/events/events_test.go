package events

// Canonical event foundation: sequence block reservation, restart safety,
// bounded replay ring, subscriber eviction, and durable/transient ordering.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// mkStore builds an isolated store with one durable session and its broker.
func mkStore(t *testing.T, key string) (*store.Sessions, *session.Managed, *Broker, *Session) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	ss, err := store.OpenSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rs := &session.RelaySession{
		ID: "0123456789abcdef0123456789abcdef", Key: key,
		Harness: session.HarnessFixture, Cwd: "/", State: session.StateIdle,
		Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := ss.Create(rs); err != nil {
		t.Fatal(err)
	}
	m := &session.Managed{Session: rs}
	b := NewBroker(ss)
	s, err := b.Ensure(m)
	if err != nil {
		t.Fatal(err)
	}
	return ss, m, b, s
}

func payload(t *testing.T, s string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"m": s})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSeqBlockReservation: the first allocation persists the watermark
// BEFORE exposing seq; multiple blocks work; restart resumes strictly
// after the persisted watermark.
func TestSeqBlockReservation(t *testing.T) {
	ss, m, _, s := mkStore(t, "seq")
	if got := m.Session.SeqHighWatermark; got != 0 {
		t.Fatalf("fresh watermark = %d", got)
	}
	ev, err := s.PublishTransient("t", payload(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 {
		t.Fatalf("first seq = %d", ev.Seq)
	}
	// Watermark persisted to the block end before seq 1 was exposed.
	if got := m.Session.SeqHighWatermark; got != SeqBlockSize {
		t.Fatalf("watermark = %d, want %d", got, SeqBlockSize)
	}
	loaded, err := ss.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded[0].SeqHighWatermark; got != SeqBlockSize {
		t.Fatalf("persisted watermark = %d", got)
	}
	// Fill the block; crossing the boundary reserves a second block.
	for i := 0; i < SeqBlockSize; i++ {
		if _, err := s.PublishTransient("t", payload(t, "fill")); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.Session.SeqHighWatermark; got != 2*SeqBlockSize {
		t.Fatalf("second block watermark = %d", got)
	}
	// Restart: a fresh broker resumes strictly AFTER the watermark.
	loaded2, _ := ss.LoadAll()
	m2 := &session.Managed{Session: loaded2[0]}
	s2, err := NewBroker(ss).Ensure(m2)
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := s2.PublishTransient("t", payload(t, "after"))
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Seq <= 2*SeqBlockSize {
		t.Fatalf("restart seq %d must exceed persisted watermark %d", ev2.Seq, 2*SeqBlockSize)
	}
}

// TestNoSeqReuseAcrossRestartTransientOnly: the highest pre-restart event
// was transient (absent from the transcript) — the restart must still
// never reuse its seq. This is the reason block reservation exists.
func TestNoSeqReuseAcrossRestartTransientOnly(t *testing.T) {
	ss, m, _, s := mkStore(t, "reuse")
	last := uint64(0)
	for i := 0; i < 5; i++ {
		ev, err := s.PublishTransient("delta", payload(t, "chunk"))
		if err != nil {
			t.Fatal(err)
		}
		last = ev.Seq
	}
	// Transcript is empty — nothing durable records `last`.
	tr, err := ss.Transcript(m.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.LastSeq() != 0 {
		t.Fatalf("transcript must be empty, last=%d", tr.LastSeq())
	}
	tr.Close()

	loaded, _ := ss.LoadAll()
	m2 := &session.Managed{Session: loaded[0]}
	s2, err := NewBroker(ss).Ensure(m2)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s2.PublishTransient("delta", payload(t, "after"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq <= last {
		t.Fatalf("seq reused: %d <= %d", ev.Seq, last)
	}
}

// TestTranscriptBeyondWatermarkFailsStartup: durable seq > watermark is
// canonical inconsistency and must fail session initialization.
func TestTranscriptBeyondWatermarkFailsStartup(t *testing.T) {
	ss, m, _, s := mkStore(t, "corrupt")
	if _, err := s.PublishDurable("x", payload(t, "1")); err != nil {
		t.Fatal(err)
	}
	// Simulate corruption: persisted watermark below the durable seq.
	bad := *m.Session
	bad.SeqHighWatermark = 0
	if err := ss.Save(&bad); err != nil {
		t.Fatal(err)
	}
	loaded, _ := ss.LoadAll()
	m2 := &session.Managed{Session: loaded[0]}
	if _, err := NewBroker(ss).Ensure(m2); err == nil {
		t.Fatal("transcript beyond watermark must fail startup")
	}
}

// TestDurablePublishStoresBeforeExposure: a durable event is appended to
// the transcript before any subscriber can observe it.
func TestDurablePublishStoresBeforeExposure(t *testing.T) {
	ss, m, _, s := mkStore(t, "durable")
	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.PublishDurable("message.agent", payload(t, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Durable {
		t.Fatal("event must be durable")
	}
	// Observable on the live queue.
	select {
	case got := <-sub.Events():
		if got.Seq != ev.Seq {
			t.Fatalf("live seq %d != %d", got.Seq, ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("durable event not delivered")
	}
	// And stored in the canonical transcript.
	tr, err := ss.Transcript(m.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	recs, through, err := tr.Tail(10)
	if err != nil || len(recs) != 1 || through != ev.Seq || recs[0].Type != "message.agent" {
		t.Fatalf("transcript: %v through=%d err=%v", recs, through, err)
	}
}

// TestTransientAbsentFromTranscript: transient events consume seq but are
// never persisted.
func TestTransientAbsentFromTranscript(t *testing.T) {
	ss, m, _, s := mkStore(t, "transient")
	ev, err := s.PublishTransient("delta", payload(t, "x"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ss.Transcript(m.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if recs, _, _ := tr.Tail(10); len(recs) != 0 {
		t.Fatalf("transient event leaked into transcript: %v", recs)
	}
	if ev.Durable {
		t.Fatal("transient event marked durable")
	}
}

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
	// Drain `fast` concurrently; never drain `slow`.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range fast.Events() {
		}
	}()
	// More events than the slow queue can hold.
	for i := 0; i < SubscriberQueueSize+50; i++ {
		if _, err := s.PublishTransient("t", payload(t, "x")); err != nil {
			t.Fatal(err)
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
	<-done
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

// TestConcurrentPublishMonotonic: concurrent publishes never produce a
// duplicate or non-monotonic seq.
func TestConcurrentPublishMonotonic(t *testing.T) {
	_, _, _, s := mkStore(t, "race")
	const n = 200
	var wg sync.WaitGroup
	seqs := make(chan uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev, err := s.PublishTransient("t", payload(t, fmt.Sprint(i)))
			if err != nil {
				t.Errorf("publish: %v", err)
				return
			}
			seqs <- ev.Seq
		}(i)
	}
	wg.Wait()
	close(seqs)
	seen := map[uint64]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Fatalf("duplicate seq %d", seq)
		}
		seen[seq] = true
	}
	if len(seen) != n {
		t.Fatalf("seq count = %d, want %d", len(seen), n)
	}
}

// TestReservationFailureDoesNotAllocate: a failed watermark save fails
// the publish closed — no seq is handed out.
func TestReservationFailureDoesNotAllocate(t *testing.T) {
	ss, m, _, s := mkStore(t, "fail")
	// Fail the metadata save (rename is the save commit point).
	ss.SetHooks(&store.Hooks{
		Rename: func(a, b string) error { return errors.New("injected") },
	})
	if _, err := s.PublishTransient("t", payload(t, "x")); err == nil {
		t.Fatal("publish must fail when reservation cannot be persisted")
	}
	if got := m.Session.SeqHighWatermark; got != 0 {
		t.Fatalf("watermark advanced on failed reservation: %d", got)
	}
}

// ---------------------------------------------------------------------
// Exact history cursor / replay floor / cursor-ahead
// ---------------------------------------------------------------------

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

// writeColdSession writes one canonical session directory directly (no
// fsync, no daemon) — the structural fixture for cold-session tests.
func writeColdSession(t *testing.T, root, key, id string, watermark uint64) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m := fmt.Sprintf(`{"version":1,"sessionId":%q,"key":%q,"harness":"fixture",`+
		`"cwd":"/","state":"idle","generation":1,"seqHighWatermark":%d,`+
		`"createdAt":%q,"updatedAt":%q}`, id, key, watermark, now, now)
	for name, data := range map[string]string{
		"session.json": m, "transcript.jsonl": "", "transcript.idx": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openFDs counts this process's open file descriptors (Linux).
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestThousandColdSessionsStructural is the cold-session resource proof:
// 1000 restored durable sessions must cost broker bookkeeping only — no
// retained transcript file descriptors and no eagerly allocated replay
// rings. Structural assertions, no timing thresholds.
func TestThousandColdSessionsStructural(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	const n = 1000
	for i := 0; i < n; i++ {
		writeColdSession(t, root, fmt.Sprintf("cold%04d", i), fmt.Sprintf("%032x", i+1), uint64(i)*2048)
	}
	ss, err := store.OpenSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ss.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != n {
		t.Fatalf("loaded %d sessions, want %d", len(loaded), n)
	}

	before := openFDs(t)
	broker := NewBroker(ss)
	for _, rs := range loaded {
		m := &session.Managed{Session: rs}
		s, err := broker.Ensure(m)
		if err != nil {
			t.Fatalf("ensure %s: %v", rs.Key, err)
		}
		if s.RingAllocated() {
			t.Fatalf("session %s eagerly allocated a replay ring", rs.Key)
		}
		if s.Cursor() != rs.SeqHighWatermark {
			t.Fatalf("session %s cursor %d != watermark %d", rs.Key, s.Cursor(), rs.SeqHighWatermark)
		}
		if s.ReplayFloor() != rs.SeqHighWatermark {
			t.Fatalf("session %s floor %d != watermark %d", rs.Key, s.ReplayFloor(), rs.SeqHighWatermark)
		}
	}
	after := openFDs(t)
	// The store and broker retain no per-session descriptors: the delta
	// must be negligible, nowhere near 2 descriptors per session.
	if after > before+32 {
		t.Fatalf("open fds grew from %d to %d for %d cold sessions", before, after, n)
	}

	// History still works on a cold session, and returns the exact
	// cursor for the live cutover (the persisted watermark).
	last := loaded[len(loaded)-1]
	s, ok := broker.Get(last.ID)
	if !ok {
		t.Fatal("session missing from broker")
	}
	page, err := s.HistorySnapshot(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != last.SeqHighWatermark || len(page.Records) != 0 {
		t.Fatalf("cold snapshot: through=%d records=%d", page.ThroughSeq, len(page.Records))
	}
	if _, err := s.Subscribe(page.ThroughSeq); err != nil {
		t.Fatalf("subscribe at cold cursor: %v", err)
	}
}

// mustMarshal is a tiny helper for sizing assertions.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// payloadOfSize returns a JSON payload of approximately n bytes.
func payloadOfSize(t *testing.T, n int) json.RawMessage {
	t.Helper()
	if n < 64 {
		t.Fatalf("payload size %d too small", n)
	}
	// {"p":"<filler>"} — filler is n minus the envelope.
	raw, err := json.Marshal(map[string]string{"p": strings.Repeat("x", n-16)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
