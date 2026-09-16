package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"path/filepath"
)

// TestRestartHistoryCursorOverHTTP is the restart replay regression end
// to end: durable seq 1 + transient seq 2 in generation 1; after restart
// the transcript endpoint reports throughSeq == the persisted watermark,
// the live subscription at that cursor works, the old durable cursor is
// CURSOR_TOO_OLD, and the future cursor is CURSOR_AHEAD.
func TestRestartHistoryCursorOverHTTP(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}

	// Generation 1.
	d1, err := Start(p, Options{SelfExe: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	served1 := make(chan error, 1)
	go func() { served1 <- d1.Serve() }()
	c1 := waitReady(t, p)
	evs1 := serveStreamer(t, d1, c1)
	if _, err := evs1.PublishDurable("message.user", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := evs1.PublishTransient("message.agent.delta", json.RawMessage(`{"chunk":"x"}`)); err != nil {
		t.Fatal(err)
	}
	m, ok := d1.registry.Get("streamer")
	if !ok {
		t.Fatal("session missing")
	}
	persisted := m.Session.SeqHighWatermark
	d1.Shutdown()
	<-served1

	// Generation 2: same durable session, fresh event state.
	d2, err := Start(p, Options{SelfExe: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	served2 := make(chan error, 1)
	go func() { served2 <- d2.Serve() }()
	defer func() { d2.Shutdown(); <-served2 }()
	c2 := waitReady(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	page, err := c2.Transcript(ctx, "streamer", 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != persisted {
		t.Fatalf("throughSeq = %d, want persisted watermark %d", page.ThroughSeq, persisted)
	}
	if len(page.Records) != 1 || page.Records[0].Seq != 1 {
		t.Fatalf("records = %+v", page.Records)
	}
	if page.HasMoreBefore {
		t.Fatal("single record must not report more-before")
	}

	// Live subscription at the snapshot cursor: exact and gap-free.
	stream, err := c2.Events(ctx, "streamer", page.ThroughSeq)
	if err != nil {
		t.Fatalf("subscribe at snapshot cursor: %v", err)
	}
	defer stream.Close()
	m2, ok := d2.registry.Get("streamer")
	if !ok {
		t.Fatal("session missing after restart")
	}
	evs2, ok := d2.broker.Get(m2.Session.ID)
	if !ok {
		t.Fatal("event state missing after restart")
	}
	next, err := evs2.PublishTransient("message.agent.delta", json.RawMessage(`{"chunk":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Next()
	if err != nil {
		t.Fatalf("live event after restart: %v", err)
	}
	if ev.Seq != next.Seq || ev.Seq <= persisted {
		t.Fatalf("live seq = %d, next = %d, watermark = %d", ev.Seq, next.Seq, persisted)
	}

	// The old durable cursor can never be served exactly again.
	if _, err := c2.Events(ctx, "streamer", 1); !apiErrCode(err, api.ErrCursorTooOld) {
		t.Fatalf("after=1: want CURSOR_TOO_OLD, got %v", err)
	}
	if _, err := c2.Events(ctx, "streamer", evs2.Cursor()+1); !apiErrCode(err, api.ErrCursorAhead) {
		t.Fatalf("after=cursor+1: want CURSOR_AHEAD, got %v", err)
	}
	if _, err := c2.Events(ctx, "streamer", ^uint64(0)); !apiErrCode(err, api.ErrCursorAhead) {
		t.Fatalf("after=MaxUint64: want CURSOR_AHEAD, got %v", err)
	}
}

// TestRestartSequenceSafety: the highest pre-restart event may be
// transient (never persisted) — the restarted daemon must still allocate
// strictly greater seqs.
func TestRestartSequenceSafety(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}

	// Generation 1: publish durable + transient, record the highest seq.
	d1, err := Start(p, Options{SelfExe: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	served1 := make(chan error, 1)
	go func() { served1 <- d1.Serve() }()
	c1 := waitReady(t, p)
	evs1 := serveStreamer(t, d1, c1)
	publishDurable(t, evs1, "message.agent")
	highest, err := evs1.PublishTransient("message.agent.delta", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// Transcript knows nothing about the transient event.
	tr, err := d1.store.Transcript(evs1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.LastSeq(); got != highest.Seq-1 {
		t.Fatalf("transcript lastSeq = %d, highest = %d", got, highest.Seq)
	}
	tr.Close()
	d1.Shutdown()
	<-served1

	// Generation 2: same durable session, new seqs strictly greater.
	d2, err := Start(p, Options{SelfExe: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	served2 := make(chan error, 1)
	go func() { served2 <- d2.Serve() }()
	defer func() { d2.Shutdown(); <-served2 }()
	c2 := waitReady(t, p)
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c2.Status(ctx, "streamer")
	if err != nil {
		t.Fatal(err)
	}
	evs2, ok := d2.broker.Get(resp.Session.SessionID)
	if !ok {
		t.Fatal("restored session has no event state")
	}
	next, err := evs2.PublishTransient("message.agent.delta", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq <= highest.Seq {
		t.Fatalf("seq reused across restart: %d <= %d", next.Seq, highest.Seq)
	}
}
