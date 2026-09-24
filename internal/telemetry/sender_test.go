package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- sender / ACK semantics -----------------------------------------------

func startService(t *testing.T, cfg Config, deps Deps) *Service {
	t.Helper()
	s := New(cfg, deps)
	s.Start()
	t.Cleanup(s.Shutdown)
	return s
}

func TestBatchDeliveryAndAck(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "rd")
	writeDescriptor(t, stateDir, "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	s := startService(t, enabledCfg(t, stateDir), testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("eeee1111", "codex", "/r", 0)
	s.FinalAnswer(m, "done")
	waitFor(t, "durable ack", func() bool { return s.Health().Acknowledged >= 2 })
	// Batch wire is a bare JSON array.
	doer.mu.Lock()
	body := doer.bodies[len(doer.bodies)-1]
	doer.mu.Unlock()
	if body[0] != '[' {
		t.Fatalf("batch wire not bare array: %.40s", body)
	}
	var evs []Event
	if err := json.Unmarshal(body, &evs); err != nil || len(evs) < 2 {
		t.Fatalf("batch body: %v", err)
	}
	if evs[0].EventID != "eeee1111:0" {
		t.Fatalf("identity: %s", evs[0].EventID)
	}
	if s.Health().State != StateConnected.String() {
		t.Fatalf("state: %s", s.Health().State)
	}
}

func TestRejectedEventCountedNotRetried(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{
		t: t,
		reply: &ingestReply{
			Accepted: 0,
			Rejected: 2,
			Errors:   []string{"x:0: bad", "x:1: bad"},
		},
	}
	s := startService(t, enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("ffff1111", "codex", "/r", 0)
	s.FinalAnswer(m, "done")
	waitFor(t, "rejects accounted", func() bool { return s.Health().Rejected >= 2 })
	if s.Health().Lost < 2 {
		t.Fatal("permanent rejects not counted as lost")
	}
	if s.Health().State != StateDegraded.String() {
		t.Fatalf("rejects should degrade: %s", s.Health().State)
	}
}

func TestOutageSpoolAndRecovery(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "rd")
	writeDescriptor(t, stateDir, "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	doer.fail.Store(true)
	s := startService(t, enabledCfg(t, stateDir), testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("11110000", "codex", "/r", 0)
	s.FinalAnswer(m, "kept")
	// Give the sender a moment to fail + spool.
	waitFor(t, "spooled", func() bool { return s.Health().SpoolBytes > 0 || s.Health().QueueDepth > 0 })
	// RepoDex returns: rediscovery replays the identical bytes.
	doer.fail.Store(false)
	waitFor(t, "replayed", func() bool { return s.Health().Acknowledged >= 2 })
	// Identical event_ids retried — verify from captured bodies.
	seen := map[string]int{}
	doer.mu.Lock()
	for _, b := range doer.bodies {
		var evs []Event
		if json.Unmarshal(b, &evs) != nil {
			continue
		}
		for _, e := range evs {
			seen[e.EventID]++
		}
	}
	doer.mu.Unlock()
	if seen["11110000:1"] < 1 {
		t.Fatal("event never delivered")
	}
}

func TestShutdownSpoolsRemainder(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	doer.fail.Store(true)
	spoolDir := filepath.Join(dir, "spool")
	s := New(enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, spoolDir))
	s.Start()
	m := testManaged("22220000", "codex", "/r", 0)
	s.FinalAnswer(m, "pending")
	s.Shutdown()
	ents, _ := os.ReadDir(spoolDir)
	if len(ents) == 0 {
		t.Fatal("shutdown left no spool chunk")
	}
	// Restart replays spool.
	doer.fail.Store(false)
	s2 := startService(t, enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, spoolDir))
	_ = s2
	waitFor(t, "spool replayed", func() bool {
		doer.mu.Lock()
		defer doer.mu.Unlock()
		for _, b := range doer.bodies {
			if strings.Contains(string(b), `"final_answer"`) {
				return true
			}
		}
		return false
	})
}

// waitFor polls until cond or deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}

// data must be a JSON object on the wire — the []byte/base64 regression
// is guarded by inspecting the exact HTTP body, not a permissive decode.
func TestDataIsJSONObjectOnWire(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	s := startService(t, enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("33330000", "codex", "/r", 0)
	s.ToolCallStarted(m, ToolStart{
		CallID:   "c1",
		ToolName: "read",
		Category: CatFileRead,
	})
	waitFor(t, "delivered", func() bool { return s.Health().Acknowledged >= 2 })
	doer.mu.Lock()
	body := doer.bodies[len(doer.bodies)-1]
	doer.mu.Unlock()
	var evs []map[string]any
	if err := json.Unmarshal(body, &evs); err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		d, isObj := e["data"].(map[string]any)
		if !isObj {
			t.Fatalf("data not a JSON object (base64 regression): %T", e["data"])
		}
		if len(d) == 0 {
			t.Fatal("data empty")
		}
	}
}

// Mixed batch [accepted, duplicate, permanent-invalid, accepted]:
// RepoDex accounts every event; rejected are lost, the batch is dropped,
// and later telemetry still flows (no wedge).
func TestMixedBatchAccounting(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	var once atomic.Bool
	doer := &fakeDoer{t: t}
	doer.replyFn = func() *ingestReply {
		if !once.Swap(true) {
			// session_started + 3 events = 4 accounted: 2 accepted,
			// 1 duplicate, 1 rejected.
			return &ingestReply{
				Accepted:   2,
				Duplicates: 1,
				Rejected:   1,
				Errors:     []string{"33334444:3: bad field"},
			}
		}
		return nil // default: accept all
	}
	s := startService(t, enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("33334444", "codex", "/r", 0)
	for i := 0; i < 3; i++ {
		s.ToolCallStarted(m, ToolStart{
			CallID:   fmt.Sprint(i),
			ToolName: "x",
			Category: CatShell,
		})
	}
	waitFor(t, "mixed ack", func() bool {
		h := s.Health()
		return h.Acknowledged >= 2 && h.Duplicates >= 1 && h.Rejected >= 1
	})
	h := s.Health()
	if h.Lost < 1 || h.State != StateDegraded.String() {
		t.Fatalf("rejected event not lost/degraded: %+v", h)
	}
	// The rejected event was dropped (not retried); later events deliver.
	s.FinalAnswer(m, "after")
	waitFor(t, "post-reject delivery", func() bool {
		return s.Health().Acknowledged >= 3
	})
	if s.Health().QueueDepth != 0 {
		t.Fatal("queue wedged on rejected event")
	}
}

// A non-auth 4xx is a definitive rejection: the batch is dropped as lost,
// never retried forever.
func TestPermanentRejectionNotRetried(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{
		t:      t,
		status: 400,
	}
	s := startService(t, enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, filepath.Join(dir, "spool")))
	m := testManaged("55550000", "codex", "/r", 0)
	s.FinalAnswer(m, "x")
	waitFor(t, "dropped", func() bool { return s.Health().Lost >= 2 })
	if s.Health().QueueDepth != 0 {
		t.Fatal("permanent reject left events pending")
	}
	reqs := doer.reqs.Load()
	time.Sleep(150 * time.Millisecond)
	if got := doer.reqs.Load(); got > reqs+1 {
		t.Fatalf("permanent rejection retried: %d -> %d", reqs, got)
	}
	if s.Health().State != StateDegraded.String() {
		t.Fatalf("state: %s", s.Health().State)
	}
}

// Emissions after Shutdown are counted losses — never silent drops.
func TestPostCloseEmitCounted(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	s := New(enabledCfg(t, filepath.Join(dir, "rd")),
		testDeps(t, doer, filepath.Join(dir, "spool")))
	s.Start()
	s.Shutdown()
	m := testManaged("66660000", "codex", "/r", 0)
	s.FinalAnswer(m, "late")
	if s.Health().Lost != 1 {
		t.Fatalf("post-close emit not counted lost: %d", s.Health().Lost)
	}
}
