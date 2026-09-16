package daemon

import (
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// codexOptions points the daemon at the deterministic fake app-server.
func codexOptions(o *Options) {
	o.CodexCommand = func() (codex.Command, error) {
		return codex.Command{Path: testBinary(), Args: []string{"__fake-codex"}}, nil
	}
}

// serveCodex creates a Codex session for a fresh temporary working
// directory.
func serveCodex(t *testing.T, c *client.Client, key string) api.SessionInfo {
	t.Helper()
	cwd := t.TempDir()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.CreateCodex(ctx, key, cwd, "", "")
	if err != nil {
		t.Fatalf("create codex session: %v", err)
	}
	return resp.Session
}

// waitDurableTypes polls the transcript until a durable record of the
// given type exists. Later records may legitimately follow it (a turn's
// terminal event precedes its input.aborted cleanup), so the wait is for
// presence, not for the tail.
func waitDurableTypes(t *testing.T, c *client.Client, key, want string) api.TranscriptPage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last api.TranscriptPage
	for time.Now().Before(deadline) {
		ctx, cancel := tctx(t)
		page, err := c.Transcript(ctx, key, 100)
		cancel()
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		last = page
		for _, r := range page.Records {
			if r.Type == want {
				return page
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("durable record %q never appeared (last=%+v)", want, last.Records)
	return last
}

// codexRoot creates an isolated HOME + RepoSuite state root.
func codexRoot(t *testing.T) (home, root string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "home")
	root = filepath.Join(base, "rs")
	for _, d := range []string{home, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home, root
}

// dialClient waits for the daemon descriptor and returns a client.
func dialClient(t *testing.T, p paths.Paths) *client.Client {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not become reachable")
	return nil
}

// listeningPorts returns every listening TCP port visible in this network
// namespace (Linux /proc). Used to prove the native app-server never opens
// a listener: it is a stdio child of relayd only.
func listeningPorts(t *testing.T) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, f := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("cannot read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err == nil {
				out[int(port)] = true
			}
		}
	}
	return out
}
