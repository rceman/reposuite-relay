package devin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/telemetry"
)

// captureDoer records every RepoDex ingest body the sender posts.
type capDoer struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capDoer) Do(r *telemetry.Request) (*telemetry.Reply, error) {
	if r.Method == "POST" {
		c.mu.Lock()
		c.bodies = append(c.bodies, r.Body)
		c.mu.Unlock()
	}
	if r.Method == "GET" {
		return &telemetry.Reply{Status: 200,
			Body: []byte(`{"schema":"reposuite.repodex.service.status.v1"}`)}, nil
	}
	var evs []json.RawMessage
	_ = json.Unmarshal(r.Body, &evs)
	rep, _ := json.Marshal(map[string]any{
		"schema": "reposuite.repodex.ingest.v1", "accepted": len(evs),
		"duplicates": 0, "rejected": 0, "errors": []string{}})
	return &telemetry.Reply{Status: 200, Body: rep}, nil
}

func (c *capDoer) events() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, b := range c.bodies {
		var evs []map[string]any
		if json.Unmarshal(b, &evs) == nil {
			out = append(out, evs...)
		}
	}
	return out
}

func telemetrySink(t *testing.T, e interface {
	Materialize(*session.Managed, harness.SessionUpdate) error
}) (*telemetry.Service, *capDoer) {
	t.Helper()
	dir := t.TempDir()
	desc := map[string]any{
		"schema": "reposuite.repodex.service.runtime.v1", "pid": 1,
		"host": "127.0.0.1", "port": 9, "instance_id": "repodex-1",
		"started_at": "2026-01-01T00:00:00Z", "repodex_version": "v",
		"protocol_version": 1}
	db, _ := json.Marshal(desc)
	if err := os.WriteFile(filepath.Join(dir, "service.runtime.json"), db, 0o600); err != nil {
		t.Fatal(err)
	}
	tok := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, "service.token"), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	cap := &capDoer{}
	svc := telemetry.New(
		telemetry.Config{Enabled: true, StateDir: dir, BatchMaxDelayMs: 10},
		telemetry.Deps{
			SpoolDir:       filepath.Join(dir, "spool"),
			AdapterVersion: "test",
			ReserveSeq: func(m *session.Managed, mark uint64) error {
				return e.Materialize(m, harness.SessionUpdate{TelemetrySeqWatermark: &mark})
			},
			HTTPClient: cap,
			Sleep:      func(time.Duration) {},
		})
	svc.Start()
	t.Cleanup(svc.Shutdown)
	return svc, cap
}

func eventsOfType(evs []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestTelemetryDevinToolAndUsage(t *testing.T) {
	e, a := newEnv(t, "tools")
	svc, cap := telemetrySink(t, e)
	a.deps.Telemetry = svc
	m := newSession(t, e, a, "k1", t.TempDir())
	if _, err := a.Prompt(context.Background(), m, text("hello")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "telemetry delivered", func() bool {
		return len(eventsOfType(cap.events(), "final_answer")) > 0 &&
			len(eventsOfType(cap.events(), "model_call_completed")) > 0
	})
	evs := cap.events()
	started := eventsOfType(evs, "tool_call_started")
	completed := eventsOfType(evs, "tool_call_completed")
	observed := eventsOfType(evs, "source_observed")
	// tc-read start+complete, tc-list start+complete, tc-exec complete.
	if len(started) != 2 || len(completed) != 3 {
		t.Fatalf("tool events: %d started %d completed", len(started), len(completed))
	}
	if len(observed) != 1 {
		t.Fatalf("source_observed: %d (listing must not observe)", len(observed))
	}
	od, _ := observed[0]["data"].(map[string]any)
	if od["observation_kind"] != "explicit_read" || od["path"] != "src/main.go" {
		t.Fatalf("observation: %+v", od)
	}
	ud, _ := eventsOfType(evs, "model_call_completed")[0]["data"].(map[string]any)
	if ud["input_tokens"] != float64(10) || ud["output_tokens"] != float64(5) ||
		ud["reasoning_tokens"] != float64(2) || ud["usage_estimated"] != true {
		t.Fatalf("usage: %+v", ud)
	}
	if evs[0]["type"] != "session_started" || evs[0]["sequence"] != float64(0) {
		t.Fatalf("first event: %+v", evs[0])
	}
	// tc-exec completion: failed status preserved, ok=false.
	for _, c := range completed {
		cd, _ := c["data"].(map[string]any)
		if cd["tool_call_id"] == "tc-exec" && cd["ok"] != false {
			t.Fatalf("failed tool marked ok: %+v", cd)
		}
	}
}
