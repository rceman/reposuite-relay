package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
)

func testManaged(id, harness, cwd string, watermark uint64) *session.Managed {
	return &session.Managed{Session: &session.RelaySession{
		ID: id, Key: "k-" + id, Harness: harness, Cwd: cwd,
		State: "idle", TelemetrySeqWatermark: watermark,
	}}
}

// fakeDoer records requests and replays scripted replies.
type fakeDoer struct {
	t       *testing.T
	reqs    atomic.Int64
	mu      sync.Mutex
	bodies  [][]byte
	fail    atomic.Bool
	status  int
	lastErr error
	reply   *ingestReply
	replyFn func() *ingestReply // per-call override; nil → default accept
}

func (f *fakeDoer) Do(r *Request) (*Reply, error) {
	f.reqs.Add(1)
	f.mu.Lock()
	f.bodies = append(f.bodies, r.Body)
	f.mu.Unlock()
	if f.fail.Load() {
		return nil, fmt.Errorf("connection refused")
	}
	if f.lastErr != nil {
		return nil, f.lastErr
	}
	if strings.HasSuffix(r.URL, "/v1/status") {
		// Discovery always answers 200 — f.status scripts the ingest
		// endpoint only.
		return &Reply{
			Status: 200,
			Body:   []byte(`{"schema":"` + statusSchema + `"}`),
		}, nil
	}
	if f.status != 0 && f.status != 200 {
		return &Reply{
			Status: f.status,
			Body:   []byte(`{}`),
		}, nil
	}
	rep := f.reply
	if f.replyFn != nil {
		rep = f.replyFn()
	}
	if rep == nil {
		var evs []json.RawMessage
		_ = json.Unmarshal(r.Body, &evs)
		rep = &ingestReply{
			Schema:   ingestSchema,
			Accepted: int64(len(evs)),
		}
	}
	rep.Schema = ingestSchema
	b, _ := json.Marshal(rep)
	return &Reply{
		Status: 200,
		Body:   b,
	}, nil
}

// writeDescriptor publishes a descriptor+token into a temp state dir.
func writeDescriptor(t *testing.T, dir, host string, port uint16) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := descriptor{
		Schema:          runtimeSchema,
		PID:             1,
		Host:            host,
		Port:            port,
		InstanceID:      "repodex-1",
		StartedAt:       "2026-01-01T00:00:00Z",
		RepoDexVersion:  "0.1.0",
		ProtocolVersion: 1,
	}
	b, _ := json.Marshal(d)
	if err := os.WriteFile(filepath.Join(dir, runtimeFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	tok := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(dir, tokenFile), []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testDeps(t *testing.T, doer Doer, spoolDir string) Deps {
	t.Helper()
	marks := map[string]uint64{}
	return Deps{
		SpoolDir:       spoolDir,
		AdapterVersion: "test",
		ReserveSeq: func(m *session.Managed, mark uint64) error {
			m.MetaMu.Lock()
			defer m.MetaMu.Unlock()
			if mark > marks[m.Session.ID] {
				marks[m.Session.ID] = mark
				m.Session.TelemetrySeqWatermark = mark
			}
			return nil
		},
		HTTPClient: doer,
		Now:        func() time.Time { return time.Now().UTC() },
		Sleep:      func(time.Duration) {},
	}
}

func enabledCfg(t *testing.T, stateDir string) Config {
	return Config{
		Enabled:  true,
		StateDir: stateDir,
	}.withDefaults()
}

// --- envelope validation -------------------------------------------------

func TestEventValidate(t *testing.T) {
	good := Event{
		Schema:    SchemaID,
		EventID:   "s1:0",
		SessionID: "s1",
		Sequence:  0,
		Timestamp: "2026-01-01T00:00:00Z",
		Source: Source{
			Runtime:        "codex",
			Adapter:        "reposuite-relay",
			AdapterVersion: "x",
		},
		Type: TypeSessionStarted,
		Data: json.RawMessage(`{}`),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good event: %v", err)
	}
	bad := good
	bad.EventID = "s1:9"
	if err := bad.Validate(); err == nil {
		t.Fatal("event_id/sequence mismatch accepted")
	}
	bad = good
	bad.Schema = "wrong"
	if err := bad.Validate(); err == nil {
		t.Fatal("bad schema accepted")
	}
	bad = good
	bad.Timestamp = "today"
	if err := bad.Validate(); err == nil {
		t.Fatal("bad timestamp accepted")
	}
}

// --- sequence / identity --------------------------------------------------

func TestSequencePerSessionAndRestart(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	doer := &fakeDoer{t: t}
	cfg := enabledCfg(t, filepath.Join(dir, "rd"))
	deps := testDeps(t, doer, filepath.Join(dir, "spool"))

	s1 := New(cfg, deps)
	m1 := testManaged("aaaa1111", "codex", "/repo/a", 0)
	m2 := testManaged("bbbb2222", "devin", "/repo/a", 0)
	s1.ToolCallStarted(m1, ToolStart{
		CallID:   "t1",
		ToolName: "x",
		Category: CatShell,
	})
	s1.ToolCallStarted(m2, ToolStart{
		CallID:   "t1",
		ToolName: "x",
		Category: CatShell,
	})
	s1.mu.Lock()
	n := len(s1.queue)
	s1.mu.Unlock()
	if n != 4 {
		t.Fatalf("want 4 queued (2 started + 2 tools), got %d", n)
	}
	// seq0 = session_started, seq1 = tool; each session independent.
	var evs []Event
	s1.mu.Lock()
	for _, q := range s1.queue {
		var e Event
		_ = json.Unmarshal(q.raw, &e)
		evs = append(evs, e)
	}
	s1.mu.Unlock()
	if evs[0].Sequence != 0 || evs[0].Type != TypeSessionStarted {
		t.Fatalf("seq0 not session_started: %+v", evs[0])
	}
	if evs[1].Sequence != 1 || evs[1].EventID != "aaaa1111:1" {
		t.Fatalf("session1 seq: %+v", evs[1])
	}
	if evs[3].SessionID != "bbbb2222" || evs[3].Sequence != 1 {
		t.Fatalf("session2 seq contaminated: %+v", evs[3])
	}

	// Daemon restart: new service, watermark durable → session_started is
	// NOT re-emitted (RepoDex dedups identical bytes but rejects drifted
	// re-emission as a permanent conflict — seq0 was allocated once).
	// The next dynamic seq resumes strictly after the watermark, never
	// reusing; the reserved block's tail is a legal crash gap.
	s2 := New(cfg, deps)
	s2.ToolCallStarted(m1, ToolStart{
		CallID:   "t2",
		ToolName: "x",
		Category: CatShell,
	})
	s2.mu.Lock()
	if len(s2.queue) != 1 {
		t.Fatalf("restart emitted %d events, want exactly the new one", len(s2.queue))
	}
	var re Event
	_ = json.Unmarshal(s2.queue[0].raw, &re)
	s2.mu.Unlock()
	if re.Type == TypeSessionStarted {
		t.Fatal("session_started re-emitted after restart")
	}
	if re.Sequence <= 256 {
		t.Fatalf("seq reused after restart: %d", re.Sequence)
	}
	if re.EventID != "aaaa1111:257" {
		t.Fatalf("event_id: %s", re.EventID)
	}
	// A session never before observed (created pre-telemetry, or a fresh
	// session) still emits session_started at seq 0 with the immutable
	// creation timestamp.
	m3 := testManaged("cccc3333", "opencode", "/repo/c", 0)
	m3.Session.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s2.FinalAnswer(m3, "hi")
	s2.mu.Lock()
	var s0 Event
	_ = json.Unmarshal(s2.queue[1].raw, &s0)
	s2.mu.Unlock()
	if s0.Sequence != 0 || s0.Type != TypeSessionStarted ||
		s0.Timestamp != "2026-01-01T00:00:00Z" {
		t.Fatalf("retroactive session_started not deterministic: %+v", s0)
	}
}

// --- queue bounds ---------------------------------------------------------

func TestQueueBoundCountsLoss(t *testing.T) {
	dir := t.TempDir()
	writeDescriptor(t, filepath.Join(dir, "rd"), "127.0.0.1", 9)
	cfg := enabledCfg(t, filepath.Join(dir, "rd"))
	cfg.QueueLimit = 4
	s := New(cfg, testDeps(t, &fakeDoer{t: t}, filepath.Join(dir, "spool")))
	m := testManaged("cccc1111", "codex", "/r", 0)
	for i := 0; i < 20; i++ {
		s.ToolCallStarted(m, ToolStart{
			CallID:   fmt.Sprint(i),
			ToolName: "x",
			Category: CatShell,
		})
	}
	h := s.Health()
	if h.Lost == 0 {
		t.Fatal("overflow not counted as loss")
	}
	if h.State != StateDegraded.String() {
		t.Fatalf("overflow should degrade: %s", h.State)
	}
}

func TestDisabledNoop(t *testing.T) {
	s := New(Config{}, testDeps(t, &fakeDoer{t: t}, t.TempDir()))
	m := testManaged("dddd1111", "codex", "/r", 0)
	s.ToolCallStarted(m, ToolStart{
		CallID:   "x",
		ToolName: "x",
		Category: CatShell,
	})
	s.FinalAnswer(m, "done")
	if h := s.Health(); h.Observed != 0 || h.Canonicalized != 0 {
		t.Fatal("disabled service observed events")
	}
	var nilSvc *Service
	nilSvc.ToolCallStarted(m, ToolStart{CallID: "x"}) // nil-safe
	nilSvc.FinalAnswer(m, "x")
	nilSvc.Shutdown()
}

// --- discovery ------------------------------------------------------------

func TestDiscoveryContract(t *testing.T) {
	dir := t.TempDir()
	if _, err := discover(dir); err == nil {
		t.Fatal("missing descriptor accepted")
	}
	writeDescriptor(t, dir, "8.8.8.8", 9999)
	if _, err := discover(dir); err == nil ||
		!strings.Contains(err.Error(), "non-loopback") {
		t.Fatal("non-loopback endpoint accepted — bearer would leak")
	}
	writeDescriptor(t, dir, "127.0.0.1", 9999)
	ep, err := discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ep.desc.Port != 9999 || ep.desc.InstanceID != "repodex-1" {
		t.Fatalf("descriptor fields: %+v", ep.desc)
	}
}
