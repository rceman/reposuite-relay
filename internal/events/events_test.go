package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
