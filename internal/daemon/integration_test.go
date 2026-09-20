package daemon_test

import (
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "relay-daemon-test-bin")
	if err != nil {
		panic(err)
	}
	testBin = filepath.Join(dir, "reposuite-relay")
	out, err := exec.Command("go", "build", "-o", testBin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build test binary: %v\n%s", err, out))
	}
	// The same-package tests share this test binary but cannot see this
	// variable — publish the path through the environment instead.
	os.Setenv("REPOSUITE_TEST_BIN", testBin)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testRoot creates isolated HOME + REPOSUITE_HOME.
func testRoot(t *testing.T) (home, root string) {
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

// cli runs the production binary under the isolated env.
func cli(t *testing.T, home, root string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(testBin, args...)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "REPOSUITE_HOME=" + root, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustCLI fails the test on a non-zero exit.
func mustCLI(t *testing.T, home, root string, args ...string) string {
	t.Helper()
	out, err := cli(t, home, root, args...)
	if err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	return out
}

var pidRe = regexp.MustCompile(`(?:^|[ =])pid=([0-9]+)`)

var daemonPidRe = regexp.MustCompile(`daemon_pid=([0-9]+)`)

func field(t *testing.T, out, name string) string {
	t.Helper()
	m := regexp.MustCompile(name + `=([^\s]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("field %s missing in %q", name, out)
	}
	return m[1]
}

func serveKey(t *testing.T, home, root, key string) (daemonPID int, fields map[string]string) {
	t.Helper()
	out := mustCLI(t, home, root, "serve", "fixture", "--key", key)
	dp, _ := strconv.Atoi(field(t, out, "daemon_pid"))
	return dp, map[string]string{
		"pid":          field(t, out, "pid"),
		"runtimeId":    field(t, out, "runtimeId"),
		"sessionId":    field(t, out, "sessionId"),
		"runtimeState": field(t, out, "runtimeState"),
		"generation":   field(t, out, "generation"),
		"state":        field(t, out, "state"),
		"harness":      field(t, out, "harness"),
	}
}

func daemonStatus(t *testing.T, home, root string) (pid int, sessionCount int) {
	t.Helper()
	out := mustCLI(t, home, root, "status", "--json")
	var rep struct {
		Running  bool `json:"running"`
		PID      int  `json:"pid"`
		Sessions int  `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if !rep.Running {
		t.Fatalf("status reports not running:\n%s", out)
	}
	return rep.PID, rep.Sessions
}

// daemonStopped asserts through `status --json` that no compatible daemon
// generation is live — the probe itself never starts one.
func daemonStopped(t *testing.T, home, root string) {
	t.Helper()
	out := mustCLI(t, home, root, "status", "--json")
	var rep struct {
		Running bool `json:"running"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if rep.Running {
		t.Fatalf("status reports a live daemon:\n%s", out)
	}
}

// alive reports whether pid exists and is not a zombie.
func alive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func waitDead(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, d)
}

func stopDaemon(t *testing.T, home, root string) {
	t.Helper()
	mustCLI(t, home, root, "daemon", "stop")
	deadline := time.Now().Add(4 * time.Second)
	desc := filepath.Join(root, "relay", "run", "daemon.json")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(desc); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("descriptor still present after daemon stop")
}

func descriptorPath(t *testing.T, home, root string) string {
	t.Helper()
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	return p.DaemonDescriptor()
}

// readDescriptor loads the runtime descriptor for raw HTTP tests (the
// token is never printed).
func readDescriptor(t *testing.T, home, root string) api.Descriptor {
	t.Helper()
	raw, err := os.ReadFile(descriptorPath(t, home, root))
	if err != nil {
		t.Fatal(err)
	}
	var d api.Descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// rawReq performs one raw HTTP request against the daemon endpoint and
// returns status + body.
func rawReq(t *testing.T, d api.Descriptor, method, path, body string, auth bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, d.Endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+d.BearerToken)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw)
}
