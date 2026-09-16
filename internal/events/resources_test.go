package events

import (
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeColdSession writes one canonical session directory directly (no
// fsync, no daemon) — the structural fixture for cold-session tests.
func writeColdSession(t *testing.T, root, key, id string, watermark uint64) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m := fmt.Sprintf(`{"version":1,"sessionId":%q,"key":%q,"harness":"fixture",`+
		`"cwd":"/","state":"idle","generation":1,"seqHighWatermark":%d,`+
		`"createdAt":%q,"updatedAt":%q}`, id, key, watermark, now, now)
	for name, data := range map[string]string{
		"session.json": m, "transcript.jsonl": "", "transcript.idx": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openFDs counts this process's open file descriptors (Linux).
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestThousandColdSessionsStructural is the cold-session resource proof:
// 1000 restored durable sessions must cost broker bookkeeping only — no
// retained transcript file descriptors and no eagerly allocated replay
// rings. Structural assertions, no timing thresholds.
func TestThousandColdSessionsStructural(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	const n = 1000
	for i := 0; i < n; i++ {
		writeColdSession(t, root, fmt.Sprintf("cold%04d", i), fmt.Sprintf("%032x", i+1), uint64(i)*2048)
	}
	ss, err := store.OpenSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ss.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != n {
		t.Fatalf("loaded %d sessions, want %d", len(loaded), n)
	}

	before := openFDs(t)
	broker := NewBroker(ss)
	for _, rs := range loaded {
		m := &session.Managed{Session: rs}
		s, err := broker.Ensure(m)
		if err != nil {
			t.Fatalf("ensure %s: %v", rs.Key, err)
		}
		if s.RingAllocated() {
			t.Fatalf("session %s eagerly allocated a replay ring", rs.Key)
		}
		if s.Cursor() != rs.SeqHighWatermark {
			t.Fatalf("session %s cursor %d != watermark %d", rs.Key, s.Cursor(), rs.SeqHighWatermark)
		}
		if s.ReplayFloor() != rs.SeqHighWatermark {
			t.Fatalf("session %s floor %d != watermark %d", rs.Key, s.ReplayFloor(), rs.SeqHighWatermark)
		}
	}
	after := openFDs(t)
	// The store and broker retain no per-session descriptors: the delta
	// must be negligible, nowhere near 2 descriptors per session.
	if after > before+32 {
		t.Fatalf("open fds grew from %d to %d for %d cold sessions", before, after, n)
	}

	// History still works on a cold session, and returns the exact
	// cursor for the live cutover (the persisted watermark).
	last := loaded[len(loaded)-1]
	s, ok := broker.Get(last.ID)
	if !ok {
		t.Fatal("session missing from broker")
	}
	page, err := s.HistorySnapshot(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != last.SeqHighWatermark || len(page.Records) != 0 {
		t.Fatalf("cold snapshot: through=%d records=%d", page.ThroughSeq, len(page.Records))
	}
	if _, err := s.Subscribe(page.ThroughSeq); err != nil {
		t.Fatalf("subscribe at cold cursor: %v", err)
	}
}

// mustMarshal is a tiny helper for sizing assertions.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// payloadOfSize returns a JSON payload of approximately n bytes.
func payloadOfSize(t *testing.T, n int) json.RawMessage {
	t.Helper()
	if n < 64 {
		t.Fatalf("payload size %d too small", n)
	}
	// {"p":"<filler>"} — filler is n minus the envelope.
	raw, err := json.Marshal(map[string]string{"p": strings.Repeat("x", n-16)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
