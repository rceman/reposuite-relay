package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/store"
	"strings"
	"testing"
	"time"
)

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
	// Count-truncated pages report more-before.
	page, err = c.Transcript(ctx, "streamer", 2)
	if err != nil || !page.HasMoreBefore || len(page.Records) != 2 {
		t.Fatalf("truncated page: %+v err=%v", page, err)
	}

	// A canonical record that alone exceeds the page byte budget is an
	// explicit bounded error, not an unbounded response.
	rec := store.Record{
		Version: store.RecordVersion, Seq: page.ThroughSeq + 1, Type: "message.agent.completed",
		At:      time.Now().UTC(),
		Payload: json.RawMessage(`{"blob":"` + strings.Repeat("x", api.HistoryPageMaxBytes+1024) + `"}`),
	}
	tr, err := d.store.Transcript(evs.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(rec); err != nil {
		tr.Close()
		t.Fatal(err)
	}
	tr.Close()
	_, err = c.Transcript(ctx, "streamer", 10)
	if !errors.As(err, &ae) || ae.Code != api.ErrHistoryRecordTooLarge {
		t.Fatalf("oversized record: want HISTORY_RECORD_TOO_LARGE, got %v", err)
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
