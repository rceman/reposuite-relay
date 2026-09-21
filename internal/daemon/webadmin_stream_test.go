// Browser control-plane streaming/cold-observer integration: the
// history→live cutover and the COLD non-wake guarantee over the admin
// cookie domain.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestWebCookieHistoryLiveCutover: browser-style stream bootstrap —
// transcript snapshot (throughSeq), an event lands between snapshot and
// subscribe, and events?after=throughSeq replays it exactly once, in
// order, over cookie-authenticated NDJSON.
func TestWebCookieHistoryLiveCutover(t *testing.T) {
	d, _, c := webDaemonCodex(t, "")
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, `{"key":"cutover","cwd":"/"}`)
	created := decodeSessionResp(t, resp)
	m, ok := d.registry.Get("cutover")
	if !ok {
		t.Fatal("session not registered")
	}
	evs, err := d.broker.Ensure(m)
	if err != nil {
		t.Fatal(err)
	}
	publishDurable(t, evs, "message.agent")
	publishDurable(t, evs, "message.agent")

	// Snapshot: throughSeq is the canonical cutover cursor.
	resp = cookieJSON(t, c, http.MethodGet, endpoint,
		"/v1/sessions/cutover/transcript?limit=50", "", "")
	var page api.TranscriptPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(page.Records) != 2 || page.ThroughSeq != 2 {
		t.Fatalf("snapshot: %+v", page)
	}
	// One durable event lands between the snapshot and the subscribe.
	intervening := publishDurable(t, evs, "message.agent")

	// Browser-style NDJSON subscription (cookie GET, no CSRF — safe method).
	req, err := http.NewRequest(http.MethodGet,
		endpoint+fmt.Sprintf("/v1/sessions/cutover/events?after=%d", page.ThroughSeq), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("events: %d", stream.StatusCode)
	}
	sc := bufio.NewScanner(stream.Body)
	sc.Buffer(make([]byte, 64<<10), api.MaxEventFrameBytes)
	var ev events.Event
	if !sc.Scan() {
		t.Fatal("stream closed before replay")
	}
	if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Seq != intervening.Seq || ev.Type != "message.agent" {
		t.Fatalf("replay prefix: seq=%d type=%s", ev.Seq, ev.Type)
	}
	// Live event after the cutover, still strictly in seq order.
	live := publishDurable(t, evs, "message.agent")
	if !sc.Scan() {
		t.Fatal("stream closed before live event")
	}
	if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Seq != live.Seq {
		t.Fatalf("live seq=%d, want %d", ev.Seq, live.Seq)
	}
	_ = created
}

// TestWebCookieColdDetailDoesNotWake: detail reads + an event subscription
// on a COLD session leave it cold — no runtime, no generation bump.
func TestWebCookieColdDetailDoesNotWake(t *testing.T) {
	d, _, c := webDaemonCodex(t, "")
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, `{"key":"coldro","cwd":"/"}`)
	resp.Body.Close()

	// Detail + transcript + a live subscription — all observer paths.
	for _, p := range []string{
		"/v1/sessions/coldro",
		"/v1/sessions/coldro/transcript?limit=200",
	} {
		resp = cookieJSON(t, c, http.MethodGet, endpoint, p, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	req, err := http.NewRequest(http.MethodGet,
		endpoint+"/v1/sessions/coldro/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()

	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/coldro", "", "")
	got := decodeSessionResp(t, resp)
	if s := got.Session; s.RuntimeState != session.RuntimeCold || s.PID != 0 || s.Generation != 0 {
		t.Fatalf("detail reads woke the session: %+v", s)
	}
}
