package daemon

import (
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
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
	resp, err := c.CreateSession(ctx, session.HarnessCodex, key, cwd, "", "")
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

// descendantListeningPorts returns listening TCP ports owned by
// descendants of this test process — relayd's spawned children (native
// harness servers, fixture children). The invariant being proven is "a
// native child never opens a listener", so only sockets owned by the
// child tree count: the daemon's own loopback control plane is owned by
// the test process itself, and unrelated listeners elsewhere in the
// network namespace cannot flake the check (Linux /proc).
func descendantListeningPorts(t *testing.T) map[int]bool {
	t.Helper()
	// LISTEN socket inode -> port.
	listen := map[string]int{}
	for _, f := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("cannot read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" { // 0A = LISTEN
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if port, err := strconv.ParseInt(portHex, 16, 32); err == nil {
				listen[fields[9]] = int(port)
			}
		}
	}
	out := map[int]bool{}
	for pid := range descendants(t) {
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue // exited meanwhile
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
				continue
			}
			if port, ok := listen[link[len("socket:["):len(link)-1]]; ok {
				out[port] = true
			}
		}
	}
	return out
}

// descendants returns the PIDs of the test process's descendant tree
// (children relayd spawned, and their children).
func descendants(t *testing.T) map[int]bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("cannot read /proc: %v", err)
	}
	parent := map[int]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		// comm is parenthesized and may itself contain ')' — ppid follows
		// the LAST ')' (field 4 after it is index 1).
		line := string(raw)
		i := strings.LastIndexByte(line, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err == nil {
			parent[pid] = ppid
		}
	}
	self := os.Getpid()
	out := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for pid, ppid := range parent {
			if pid != self && !out[pid] && (ppid == self || out[ppid]) {
				out[pid] = true
				changed = true
			}
		}
	}
	return out
}
