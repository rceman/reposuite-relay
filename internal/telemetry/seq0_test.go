package telemetry

// seq0 (session_started) crash-window and replay-stability tests.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seq0 must survive a crash between allocation and RepoDex ACK: the
// canonical event bytes are frozen into durable session metadata, so a
// restart replays the identical event — RepoDex accepts it as new.
// The crashed generation's queue is memory-only and never reaches the
// spool — recovery must not depend on either.
func TestSeq0CrashRecovery(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	spoolDir := filepath.Join(dir, "spool")
	deps := testDeps(t, doer, spoolDir)
	cfg := enabledCfg(t, filepath.Join(dir, "rd"))
	m := testManaged("77770000", "codex", "/r", 0)
	m.Session.CreatedAt = time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

	// Generation 1: emit then crash — no Start, so nothing ever reached
	// RepoDex or the spool; the in-memory queue is lost.
	s1 := New(cfg, deps)
	s1.FinalAnswer(m, "lost")
	frozen := m.Session.TelemetryStartedEvent
	if frozen == "" {
		t.Fatal("seq0 canonical bytes not frozen durably")
	}
	if m.Session.TelemetrySeqWatermark != seqBlockSize {
		t.Fatalf("watermark: %d", m.Session.TelemetrySeqWatermark)
	}
	ents, _ := os.ReadDir(spoolDir)
	if len(ents) != 0 {
		t.Fatal("crash simulation must not have spooled seq0")
	}

	// Generation 2: only durable state survived. First observation must
	// replay the FROZEN seq0 verbatim.
	s2 := startService(t, cfg, deps)
	s2.ToolCallStarted(m, ToolStart{
		CallID:   "t1",
		ToolName: "x",
		Category: CatShell,
	})
	waitFor(t, "seq0 delivered", func() bool { return s2.Health().Acknowledged >= 2 })
	h := s2.Health()
	if h.Rejected != 0 || h.Lost != 0 || h.Duplicates != 0 {
		t.Fatalf("recoverable seq0 counted as failure: %+v", h)
	}
	doer.mu.Lock()
	var seq0raw []byte
	for _, b := range doer.bodies {
		var evs []json.RawMessage
		if json.Unmarshal(b, &evs) != nil {
			continue
		}
		for _, e := range evs {
			if eventIDOf(e) == "77770000:0" {
				seq0raw = e
			}
		}
	}
	doer.mu.Unlock()
	if string(seq0raw) != frozen {
		t.Fatalf("replayed seq0 != frozen canonical bytes")
	}
	var ev Event
	_ = json.Unmarshal(seq0raw, &ev)
	if ev.Sequence != 0 || ev.EventID != "77770000:0" ||
		ev.Timestamp != "2026-09-20T08:00:00Z" {
		t.Fatalf("seq0 identity drifted: %+v", ev)
	}
}

// After RepoDex durably ACKed seq0, a restart replays the frozen bytes —
// RepoDex reports Duplicate (same event_id + same digest), which Relay
// treats as success: no rejection, no loss, no degraded state.
func TestSeq0AckedRestartDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	deps := testDeps(t, doer, filepath.Join(dir, "spool"))
	cfg := enabledCfg(t, filepath.Join(dir, "rd"))
	m := testManaged("88880000", "codex", "/r", 0)

	s1 := startService(t, cfg, deps)
	s1.FinalAnswer(m, "done")
	waitFor(t, "gen1 ack", func() bool { return s1.Health().Acknowledged >= 2 })
	s1.Shutdown()
	frozen := m.Session.TelemetryStartedEvent

	s2 := startService(t, cfg, deps)
	s2.FinalAnswer(m, "again")
	waitFor(t, "gen2 dup", func() bool { return s2.Health().Duplicates >= 1 })
	h := s2.Health()
	if h.Rejected != 0 || h.Lost != 0 {
		t.Fatalf("duplicate seq0 counted as failure: %+v", h)
	}
	if h.State == StateDegraded.String() {
		t.Fatal("deduped replay degraded the pipeline")
	}
	// The replayed bytes are identical — same event_id, same digest.
	doer.mu.Lock()
	var last []byte
	for _, b := range doer.bodies {
		var evs []json.RawMessage
		if json.Unmarshal(b, &evs) != nil {
			continue
		}
		for _, e := range evs {
			if eventIDOf(e) == "88880000:0" {
				last = e
			}
		}
	}
	doer.mu.Unlock()
	if string(last) != frozen {
		t.Fatal("replayed seq0 drifted from frozen bytes")
	}
}

// Frozen seq0 must not observe mutable state: changing native identity,
// model, mode, project grouping, and even the adapter version between
// generations must replay byte-identical canonical bytes.
func TestSeq0FrozenAgainstMutation(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	spoolDir := filepath.Join(dir, "spool")
	deps := testDeps(t, doer, spoolDir)
	cfg := enabledCfg(t, filepath.Join(dir, "rd"))
	m := testManaged("99990000", "codex", "/r", 0)

	s1 := New(cfg, deps)
	s1.FinalAnswer(m, "x")
	frozen := m.Session.TelemetryStartedEvent

	// Mutate every mutable fact a session/relay can accumulate.
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "thr_later"
	m.Session.Model = "newer-model"
	m.Session.Mode = "high"
	m.MetaMu.Unlock()
	deps.AdapterVersion = "9.9.9-different"
	deps.ProjectID = func(string) string { return "proj-changed" }

	s2 := New(cfg, deps)
	s2.ToolCallStarted(m, ToolStart{
		CallID:   "t",
		ToolName: "x",
		Category: CatShell,
	})
	s2.mu.Lock()
	var replayed string
	for _, q := range s2.queue {
		if q.id == "99990000:0" {
			replayed = string(q.raw)
		}
	}
	s2.mu.Unlock()
	if replayed != frozen {
		t.Fatalf("seq0 observed mutable state:\n%s\nvs\n%s", replayed, frozen)
	}
	// And the frozen content itself carries no mutable fields.
	var ev Event
	_ = json.Unmarshal([]byte(frozen), &ev)
	if ev.Source.NativeSessionID != "" || ev.ProjectID != "" {
		t.Fatalf("seq0 embeds mutable fields: %+v", ev)
	}
}
