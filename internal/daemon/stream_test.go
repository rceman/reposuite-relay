package daemon

// Canonical event stream over HTTP: NDJSON replay-then-live, cursor
// handling, subscriber lifecycle, transcript endpoint, history/live
// cutover, and restart sequence safety. Synthetic events only — no real
// harness, no model quota.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// serveStreamer creates one fixture session and returns its event state.
func serveStreamer(t *testing.T, d *Daemon, c *client.Client) *events.Session {
	t.Helper()
	sess, err := serve(t, c, "streamer")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	evs, ok := d.broker.Get(sess.SessionID)
	if !ok {
		t.Fatal("event state missing for managed session")
	}
	return evs
}

func publishDurable(t *testing.T, evs *events.Session, typ string) events.Event {
	t.Helper()
	ev, err := evs.PublishDurable(typ, json.RawMessage(`{"n":1}`))
	if err != nil {
		t.Fatalf("publish durable: %v", err)
	}
	return ev
}

// TestEventStreamReplayThenLive: replay events first, then live events,
// each one valid NDJSON with strictly increasing seq; disconnect releases
// the subscriber.
func TestEventStreamReplayThenLive(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	for i := 0; i < 3; i++ {
		publishDurable(t, evs, "message.agent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Events(ctx, "streamer", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	last := uint64(0)
	for i := 0; i < 3; i++ {
		ev, err := stream.Next()
		if err != nil {
			t.Fatalf("replay event %d: %v", i, err)
		}
		if ev.Seq <= last {
			t.Fatalf("seq not increasing: %d then %d", last, ev.Seq)
		}
		last = ev.Seq
		if !ev.Durable || ev.SessionID == "" {
			t.Fatalf("malformed event: %+v", ev)
		}
	}
	// Live transient event follows.
	live, err := evs.PublishTransient("message.agent.delta", json.RawMessage(`{"chunk":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Next()
	if err != nil {
		t.Fatalf("live event: %v", err)
	}
	if ev.Seq != live.Seq || ev.Durable {
		t.Fatalf("live event mismatch: %+v (want seq %d transient)", ev, live.Seq)
	}
	// Disconnect releases the subscriber deterministically.
	cancel()
	stream.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if evs.SubscriberCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber leaked after disconnect: %d", evs.SubscriberCount())
}

// TestEventStreamAuthRequired: a wrong token never establishes a
// subscriber.
func TestEventStreamAuthRequired(t *testing.T) {
	d, p, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	desc, err := readDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, desc.Endpoint+"/v1/sessions/streamer/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+"0"+desc.BearerToken[1:])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 401 || !strings.Contains(string(body), "UNAUTHORIZED") {
		t.Fatalf("stream without valid auth: %d %s", resp.StatusCode, body)
	}
	if evs.SubscriberCount() != 0 {
		t.Fatal("unauthenticated request registered a subscriber")
	}
}

// TestEventStreamCursorTooOld: an unreplayable cursor is a normal JSON
// error before any streaming begins.
func TestEventStreamCursorTooOld(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	for i := 0; i < events.ReplayRingSize+5; i++ {
		if _, err := evs.PublishTransient("t", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := tctx(t)
	defer cancel()
	_, err := c.Events(ctx, "streamer", 0)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Code != api.ErrCursorTooOld {
		t.Fatalf("want CURSOR_TOO_OLD, got %v", err)
	}
	if evs.SubscriberCount() != 0 {
		t.Fatal("failed subscribe registered a subscriber")
	}
}

// TestTranscriptEndpoint: bounded durable tail through the derived index.
func TestTranscriptEndpoint(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	for i := 0; i < 5; i++ {
		publishDurable(t, evs, "message.agent")
	}
	ctx, cancel := tctx(t)
	defer cancel()
	page, err := c.Transcript(ctx, "streamer", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != 5 || len(page.Records) != 2 {
		t.Fatalf("page through=%d records=%d", page.ThroughSeq, len(page.Records))
	}
	// limit=0 → no records but throughSeq still current.
	page, err = c.Transcript(ctx, "streamer", 0)
	if err != nil || len(page.Records) != 0 || page.ThroughSeq != 5 {
		t.Fatalf("limit=0: %+v %v", page, err)
	}
	// Oversized limit is clamped server-side; small transcript unaffected.
	page, err = c.Transcript(ctx, "streamer", 100000)
	if err != nil || len(page.Records) != 5 {
		t.Fatalf("clamped limit: %+v %v", page, err)
	}
	// Negative limit is a bounded invalid request.
	_, err = c.Transcript(ctx, "streamer", -1)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Code != api.ErrInvalidRequest {
		t.Fatalf("negative limit: %v", err)
	}
	// Missing session is a stable 404.
	_, err = c.Transcript(ctx, "nope", 10)
	if !errors.As(err, &ae) || ae.Code != api.ErrSessionNotFound {
		t.Fatalf("missing session: %v", err)
	}
}

// TestHistoryLiveCutover: hydrate via transcript (throughSeq=N), then
// subscribe after=N — the intervening event arrives from replay, then
// live events follow. No gap, no duplicate.
func TestHistoryLiveCutover(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	for i := 0; i < 4; i++ {
		publishDurable(t, evs, "message.agent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	page, err := c.Transcript(ctx, "streamer", 100)
	if err != nil {
		t.Fatal(err)
	}
	n := page.ThroughSeq
	if n != 4 {
		t.Fatalf("throughSeq = %d", n)
	}
	// An event occurs between hydration and subscribe — durable...
	intervening := publishDurable(t, evs, "message.agent")
	// ...and a transient one too (still inside the replay window).
	if _, err := evs.PublishTransient("message.agent.delta", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Events(ctx, "streamer", n)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next()
	if err != nil || first.Seq != intervening.Seq {
		t.Fatalf("cutover gap: %v %+v", err, first)
	}
	second, err := stream.Next()
	if err != nil || second.Seq != intervening.Seq+1 {
		t.Fatalf("cutover transient: %v %+v", err, second)
	}
	// Live events continue after the cutover.
	live := publishDurable(t, evs, "message.agent")
	third, err := stream.Next()
	if err != nil || third.Seq != live.Seq {
		t.Fatalf("live after cutover: %v %+v", err, third)
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

// --- small helpers ----------------------------------------------------

func waitReady(t *testing.T, p paths.Paths) *client.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not become ready")
	return nil
}

// (no further helpers)
