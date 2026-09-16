package daemon

// COLD-session resource behavior at the daemon level: 1000 restored
// durable sessions must not cost per-session file descriptors or replay
// rings, and daemon startup must remain structurally cheap.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/paths"
)

// writeColdSession writes one canonical session directory directly.
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

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestThousandColdSessionsDaemonStart: a daemon restoring 1000 durable
// COLD sessions starts, lists them, and retains no per-session file
// descriptors; event state holds no replay rings until a publish.
func TestThousandColdSessionsDaemonStart(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, d := range []string{home, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionsDir := p.SessionsDir()
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const n = 1000
	for i := 0; i < n; i++ {
		writeColdSession(t, sessionsDir, fmt.Sprintf("cold%04d", i),
			fmt.Sprintf("%032x", i+1), uint64(i)*1024)
	}

	before := openFDCount(t)
	d, err := Start(p, Options{SelfExe: "/bin/true"})
	if err != nil {
		t.Fatalf("start with %d cold sessions: %v", n, err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	defer func() { d.Shutdown(); <-served }()
	c := waitReady(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := c.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != n {
		t.Fatalf("listed %d sessions, want %d", len(resp.Sessions), n)
	}
	after := openFDCount(t)
	if after > before+64 {
		t.Fatalf("open fds grew from %d to %d across daemon start + list of %d cold sessions",
			before, after, n)
	}
	// Every restored session projects COLD and carries its cursor.
	for _, s := range []int{0, n / 2, n - 1} {
		if resp.Sessions[s].RuntimeState != "cold" {
			t.Fatalf("session %d not cold: %s", s, resp.Sessions[s].RuntimeState)
		}
	}
	// History on a cold session returns the exact cursor and spawns no
	// runtime.
	page, err := c.Transcript(ctx, "cold0000", 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != 0 || len(page.Records) != 0 {
		t.Fatalf("cold history: through=%d records=%d", page.ThroughSeq, len(page.Records))
	}
	page, err = c.Transcript(ctx, fmt.Sprintf("cold%04d", n-1), 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.ThroughSeq != uint64(n-1)*1024 {
		t.Fatalf("cold history cursor = %d, want %d", page.ThroughSeq, uint64(n-1)*1024)
	}
}

// TestColdSessionEventStateStructural: managed sessions that never
// published hold no replay ring.
func TestColdSessionEventStateStructural(t *testing.T) {
	d, _, c, _ := startInProcess(t, nil)
	for _, key := range []string{"a", "b"} {
		if _, err := serve(t, c, key); err != nil {
			t.Fatal(err)
		}
	}
	sessions := d.broker.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("broker sessions = %d", len(sessions))
	}
	for _, s := range sessions {
		if s.RingAllocated() {
			t.Fatal("managed session allocated a ring without publishing")
		}
	}
}
