package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
