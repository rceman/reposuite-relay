package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/paths"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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

// wrongToken returns a token GUARANTEED to differ from the real one. (The
// obvious "flip the first character to 0" is a 1-in-16 flake for a hex token:
// it can produce the valid token and hang the stream forever.)
func wrongToken(real string) string {
	if real == "" {
		return "x"
	}
	last := real[len(real)-1]
	replacement := byte('0')
	if last == '0' {
		replacement = '1'
	}
	return real[:len(real)-1] + string(replacement)
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
	req.Header.Set("Authorization", "Bearer "+wrongToken(desc.BearerToken))
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

// TestEventStreamCursorAhead: a cursor above the current canonical event
// cursor is refused deterministically — including the maximum uint64 —
// and never registers a subscriber.
func TestEventStreamCursorAhead(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	evs := serveStreamer(t, d, c)
	ctx, cancel := tctx(t)
	defer cancel()

	// Fresh session: cursor is 0, so after=1 is from the future.
	_, err := c.Events(ctx, "streamer", 1)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Code != api.ErrCursorAhead {
		t.Fatalf("want CURSOR_AHEAD, got %v", err)
	}
	if _, err := c.Events(ctx, "streamer", ^uint64(0)); !errors.As(err, &ae) || ae.Code != api.ErrCursorAhead {
		t.Fatalf("want CURSOR_AHEAD for MaxUint64, got %v", err)
	}
	if evs.SubscriberCount() != 0 {
		t.Fatal("rejected cursor registered a subscriber")
	}
	// At the cursor exactly is valid (live-only subscription).
	stream, err := c.Events(ctx, "streamer", evs.Cursor())
	if err != nil {
		t.Fatalf("subscribe at cursor: %v", err)
	}
	stream.Close()
}

// apiErrCode reports whether err is an APIError with the given code.
func apiErrCode(err error, code string) bool {
	var ae *client.APIError
	return errors.As(err, &ae) && ae.Code == code
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
