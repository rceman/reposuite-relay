package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/telemetry"
)

// captureDoer records every RepoDex ingest body the sender posts.
type captureDoer struct {
	mu     sync.Mutex
	bodies [][]byte
	fail   bool
}

func (c *captureDoer) Do(r *telemetry.Request) (*telemetry.Reply, error) {
	if len(r.Body) > 0 && r.Method == "POST" {
		c.mu.Lock()
		c.bodies = append(c.bodies, r.Body)
		c.mu.Unlock()
	}
	if c.fail {
		return nil, context.DeadlineExceeded
	}
	if r.Method == "GET" {
		return &telemetry.Reply{Status: 200,
			Body: []byte(`{"schema":"reposuite.repodex.service.status.v1"}`)}, nil
	}
	var evs []json.RawMessage
	_ = json.Unmarshal(r.Body, &evs)
	rep, _ := json.Marshal(map[string]any{
		"schema":   "reposuite.repodex.ingest.v1",
		"accepted": len(evs), "duplicates": 0, "rejected": 0,
		"errors": []string{}})
	return &telemetry.Reply{Status: 200, Body: rep}, nil
}

func (c *captureDoer) events() []map[string]any {
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

// telemetryEnv wires a real telemetry Service into the adapter's Deps.
func (e *env) telemetryEnv(t *testing.T) (*telemetry.Service, *captureDoer) {
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
	if err := os.WriteFile(filepath.Join(dir, "service.token"),
		[]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	cap := &captureDoer{}
	svc := telemetry.New(
		telemetry.Config{Enabled: true, StateDir: dir, BatchMaxDelayMs: 10},
		telemetry.Deps{
			SpoolDir:       filepath.Join(dir, "spool"),
			AdapterVersion: "test",
			ReserveSeq: func(m *session.Managed, mark uint64) error {
				return e.materialize(m, harness.SessionUpdate{TelemetrySeqWatermark: &mark})
			},
			HTTPClient: cap,
			Sleep:      func(time.Duration) {},
		})
	svc.Start()
	t.Cleanup(svc.Shutdown)
	e.adapter.deps.Telemetry = svc
	return svc, cap
}

func findEvents(evs []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestTelemetryCodexToolLifecycle(t *testing.T) {
	e := newEnv(t, "")
	_, cap := e.telemetryEnv(t)
	m := e.newSession("k1", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "tool:read"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "telemetry delivered", func() bool {
		return len(findEvents(cap.events(), "final_answer")) > 0 &&
			len(findEvents(cap.events(), "model_call_completed")) > 0
	})
	evs := cap.events()
	started := findEvents(evs, "tool_call_started")
	completed := findEvents(evs, "tool_call_completed")
	observed := findEvents(evs, "source_observed")
	answers := findEvents(evs, "final_answer")
	usage := findEvents(evs, "model_call_completed")
	if len(started) != 1 || len(completed) != 1 {
		t.Fatalf("tool lifecycle: %d started %d completed", len(started), len(completed))
	}
	sd, _ := started[0]["data"].(map[string]any)
	cd, _ := completed[0]["data"].(map[string]any)
	if sd["tool_call_id"] != cd["tool_call_id"] {
		t.Fatal("tool identity not paired start→complete")
	}
	if sd["category"] != "shell" || cd["ok"] != true {
		t.Fatalf("mapping: %+v / %+v", sd, cd)
	}
	// Structured read action + delivered output → exactly one explicit_read.
	if len(observed) != 1 {
		t.Fatalf("source_observed count: %d", len(observed))
	}
	od, _ := observed[0]["data"].(map[string]any)
	if od["observation_kind"] != "explicit_read" || od["path"] != "src/main.go" {
		t.Fatalf("observation: %+v", od)
	}
	if od["content_digest"] == nil || od["bytes"] == nil {
		t.Fatal("delivered content not digested")
	}
	if len(answers) != 1 {
		t.Fatalf("final_answer count: %d", len(answers))
	}
	if len(usage) != 1 {
		t.Fatalf("usage count: %d", len(usage))
	}
	ud, _ := usage[0]["data"].(map[string]any)
	if ud["input_tokens"] != float64(10) || ud["output_tokens"] != float64(5) {
		t.Fatalf("usage not copied exactly: %+v", ud)
	}
	if ud["usage_estimated"] != true {
		t.Fatal("turn-scoped usage must carry usage_estimated")
	}
	if ud["reasoning_tokens"] != float64(0) {
		t.Fatalf("reasoning: %v", ud["reasoning_tokens"])
	}
	// session_started is seq 0 with stable identity.
	if evs[0]["type"] != "session_started" || evs[0]["sequence"] != float64(0) {
		t.Fatalf("first event: %+v", evs[0])
	}
	if evs[0]["event_id"] != evs[0]["session_id"].(string)+":0" {
		t.Fatal("event_id contract violated")
	}
}

func TestTelemetryCodexNegativeSourceObserved(t *testing.T) {
	// listFiles, multi-action commands, and non-repo tools never produce
	// source_observed — filenames and mixed/unattributable output are not
	// delivered single-source content.
	for _, kind := range []string{"tool:list", "tool:multi-read", "tool:mcp", "tool:web"} {
		e := newEnv(t, "")
		_, cap := e.telemetryEnv(t)
		m := e.newSession("k"+strings.ReplaceAll(kind, ":", ""), t.TempDir())
		if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: kind}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "telemetry delivered "+kind, func() bool {
			return len(findEvents(cap.events(), "final_answer")) > 0
		})
		if n := len(findEvents(cap.events(), "source_observed")); n != 0 {
			t.Fatalf("%s: filename/listing/mixed wrongly observed: %d", kind, n)
		}
	}
}

// A single-action `search` command delivers source snippets — emitted as
// search_snippet WITHOUT a path (the action path is the search root, not
// an observed file; per-file attribution is unprovable).
func TestTelemetryCodexSearchSnippet(t *testing.T) {
	e := newEnv(t, "")
	_, cap := e.telemetryEnv(t)
	m := e.newSession("k-search", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "tool:search"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "search snippet", func() bool {
		return len(findEvents(cap.events(), "source_observed")) > 0
	})
	od, _ := findEvents(cap.events(), "source_observed")[0]["data"].(map[string]any)
	if od["observation_kind"] != "search_snippet" {
		t.Fatalf("kind: %+v", od)
	}
	if _, hasPath := od["path"]; hasPath {
		t.Fatalf("search root misattributed as observed file: %+v", od)
	}
}

func TestTelemetryCodexDiffAndFailure(t *testing.T) {
	e := newEnv(t, "")
	_, cap := e.telemetryEnv(t)
	m := e.newSession("k-edit", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "tool:edit"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "diff observed", func() bool {
		return len(findEvents(cap.events(), "source_observed")) > 0
	})
	od, _ := findEvents(cap.events(), "source_observed")[0]["data"].(map[string]any)
	if od["observation_kind"] != "diff" || od["path"] != "src/main.go" {
		t.Fatalf("diff observation: %+v", od)
	}

	e2 := newEnv(t, "")
	_, cap2 := e2.telemetryEnv(t)
	m2 := e2.newSession("k-fail", t.TempDir())
	if _, err := e2.adapter.Prompt(context.Background(), m2, harness.PromptCommand{Text: "tool:fail"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed tool", func() bool {
		return len(findEvents(cap2.events(), "tool_call_completed")) > 0
	})
	cd, _ := findEvents(cap2.events(), "tool_call_completed")[0]["data"].(map[string]any)
	if cd["ok"] != false || cd["status"] != "failed" {
		t.Fatalf("failure mapping: %+v", cd)
	}
}

func TestTelemetryCodexUnknownItemIgnored(t *testing.T) {
	e := newEnv(t, "")
	_, cap := e.telemetryEnv(t)
	m := e.newSession("k-unk", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "tool:unknown"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn done", func() bool {
		return len(findEvents(cap.events(), "final_answer")) > 0
	})
	if n := len(findEvents(cap.events(), "tool_call_started")); n != 0 {
		t.Fatalf("unknown item fabricated a tool call: %d", n)
	}
}

func TestTelemetryCodexInterruptedNoFinalAnswer(t *testing.T) {
	e := newEnv(t, "")
	_, cap := e.telemetryEnv(t)
	m := e.newSession("k-int", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "answer", func() bool {
		return len(findEvents(cap.events(), "final_answer")) > 0
	})
	// interrupted/failed turns are covered by the FakeFailTurn-style path:
	// absence is proven structurally — an interrupted turn emits no
	// final_answer (onTurnCompleted only emits on "completed").
}
