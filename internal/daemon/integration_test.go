package daemon_test

// End-to-end tests for the daemon/session milestone. Every test uses an
// isolated temporary REPOSUITE_HOME + HOME; nothing touches real user
// state, ~/.airelay, or live processes outside the PIDs the test created.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
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
	out := mustCLI(t, home, root, "daemon", "status")
	p, _ := strconv.Atoi(field(t, out, "pid"))
	n, _ := strconv.Atoi(field(t, out, "sessionCount"))
	return p, n
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

// TestDaemonAbsentByDefault: no autostart for status/stop.
func TestDaemonAbsentByDefault(t *testing.T) {
	home, root := testRoot(t)
	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("daemon status on empty root must fail: %s", out)
	}
	if out, err := cli(t, home, root, "daemon", "stop"); err == nil {
		t.Fatalf("daemon stop on empty root must fail: %s", out)
	}
}

// TestTwoSessionIsolation is the core acceptance test.
func TestTwoSessionIsolation(t *testing.T) {
	home, root := testRoot(t)
	dpA, a := serveKey(t, home, root, "alpha")
	dpB, b := serveKey(t, home, root, "beta")

	if dpA != dpB {
		t.Fatalf("different daemons: %d vs %d", dpA, dpB)
	}
	if a["runtimeId"] == b["runtimeId"] || a["pid"] == b["pid"] {
		t.Fatalf("sessions not isolated: %v / %v", a, b)
	}
	if a["generation"] != "1" || b["generation"] != "1" {
		t.Fatalf("generations: %v / %v", a, b)
	}
	pidA, _ := strconv.Atoi(a["pid"])
	pidB, _ := strconv.Atoi(b["pid"])
	if !alive(pidA) || !alive(pidB) {
		t.Fatal("fixture children not alive")
	}

	mustCLI(t, home, root, "stop", "alpha")
	waitDead(t, pidA, 3*time.Second)
	if out, err := cli(t, home, root, "status", "alpha"); err == nil {
		t.Fatalf("status alpha after stop must fail: %s", out)
	}
	out := mustCLI(t, home, root, "list")
	if strings.Contains(out, "key=alpha") {
		t.Fatalf("alpha still listed: %s", out)
	}

	// B is untouched.
	if !alive(pidB) {
		t.Fatal("beta died with alpha")
	}
	sb := mustCLI(t, home, root, "status", "beta")
	for k, want := range b {
		if got := field(t, sb, k); got != want {
			t.Fatalf("beta %s changed: %s -> %s", k, want, got)
		}
	}

	mustCLI(t, home, root, "stop", "beta")
	waitDead(t, pidB, 3*time.Second)
	stopDaemon(t, home, root)
}

// TestSingletonRace: N concurrent CLI clients, one fresh root, one daemon.
func TestSingletonRace(t *testing.T) {
	home, root := testRoot(t)
	const n = 16
	type res struct {
		out string
		err error
	}
	results := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := cli(t, home, root, "serve", "fixture", "--key", fmt.Sprintf("worker_%02d", i))
			results[i] = res{out, err}
		}(i)
	}
	wg.Wait()

	daemonPIDs := map[string]bool{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker_%02d failed: %v\n%s", i, r.err, r.out)
		}
		daemonPIDs[field(t, r.out, "daemon_pid")] = true
	}
	if len(daemonPIDs) != 1 {
		t.Fatalf("clients saw %d daemons: %v", len(daemonPIDs), daemonPIDs)
	}
	dp, count := daemonStatus(t, home, root)
	if !daemonPIDs[strconv.Itoa(dp)] || count != n {
		t.Fatalf("daemon status pid=%d count=%d", dp, count)
	}
	// Exactly one descriptor generation: every client converged on the
	// same instance identity and loopback endpoint.
	desc := readDescriptor(t, home, root)
	if len(desc.InstanceID) != 32 {
		t.Fatalf("instanceId malformed: %q", desc.InstanceID)
	}
	if !strings.HasPrefix(desc.Endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint not loopback: %q", desc.Endpoint)
	}
	if desc.PID != dp {
		t.Fatalf("descriptor pid=%d status pid=%d", desc.PID, dp)
	}
	// Only one daemon process owns the lock; a status command does not
	// spawn anything new.
	dp2, _ := daemonStatus(t, home, root)
	if dp2 != dp {
		t.Fatalf("daemon status spawned a second daemon: %d -> %d", dp, dp2)
	}
	out := mustCLI(t, home, root, "list")
	for i := 0; i < n; i++ {
		if !strings.Contains(out, fmt.Sprintf("key=worker_%02d", i)) {
			t.Fatalf("worker_%02d missing: %s", i, out)
		}
	}
	stopDaemon(t, home, root)
}

// TestDuplicateKey: SESSION_EXISTS; original untouched; one child.
func TestDuplicateKey(t *testing.T) {
	home, root := testRoot(t)
	_, first := serveKey(t, home, root, "same")
	out, err := cli(t, home, root, "serve", "fixture", "--key", "same")
	if err == nil {
		t.Fatalf("duplicate serve must fail: %s", out)
	}
	if !strings.Contains(out, "SESSION_EXISTS") {
		t.Fatalf("want SESSION_EXISTS, got: %s", out)
	}
	sb := mustCLI(t, home, root, "status", "same")
	if field(t, sb, "runtimeId") != first["runtimeId"] || field(t, sb, "pid") != first["pid"] {
		t.Fatalf("original session mutated: %s", sb)
	}
	_, count := daemonStatus(t, home, root)
	if count != 1 {
		t.Fatalf("sessionCount=%d", count)
	}
	stopDaemon(t, home, root)
}

// TestKeyValidationCLI: malformed keys rejected client-side and daemon-side.
func TestKeyValidationCLI(t *testing.T) {
	home, root := testRoot(t)
	for _, bad := range []string{"a/b", "a b", "", ".x", "key!", strings.Repeat("k", 65)} {
		args := []string{"serve", "fixture", "--key", bad}
		if out, err := cli(t, home, root, args...); err == nil {
			t.Fatalf("key %q accepted: %s", bad, out)
		}
	}
}

// TestStaleDescriptorRecovery: a crashed daemon leaves a stale descriptor
// (dead endpoint). A managed command spawns a contender; the lock owner
// atomically replaces the descriptor with its own generation.
func TestStaleDescriptorRecovery(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	stale := api.Descriptor{
		Version:     api.DescriptorVersion,
		InstanceID:  strings.Repeat("a", 32),
		PID:         999999,
		Endpoint:    "http://127.0.0.1:1", // nothing listens here
		BearerToken: strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(p.DaemonDescriptor(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// A managed command must recover and start the daemon.
	mustCLI(t, home, root, "list")
	_, count := daemonStatus(t, home, root)
	_ = count
	fresh := readDescriptor(t, home, root)
	if fresh.InstanceID == stale.InstanceID {
		t.Fatal("stale descriptor was not replaced by the lock owner")
	}
	stopDaemon(t, home, root)
}

// TestMalformedDescriptorFailsClosed: a corrupt descriptor is never
// trusted and never silently deleted — the client fails closed as a bad
// peer rather than sending commands somewhere unknown.
func TestMalformedDescriptorFailsClosed(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	marker := []byte("{not a descriptor")
	if err := os.WriteFile(p.DaemonDescriptor(), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, home, root, "list")
	if err == nil {
		t.Fatalf("malformed descriptor accepted: %s", out)
	}
	if !strings.Contains(out, "compatible relayd") {
		t.Fatalf("want fail-closed bad-peer error, got: %s", out)
	}
	got, _ := os.ReadFile(p.DaemonDescriptor())
	if string(got) != string(marker) {
		t.Fatal("client modified/deleted the descriptor")
	}
	// Non-loopback endpoint also fails closed.
	bad := api.Descriptor{
		Version: api.DescriptorVersion, InstanceID: strings.Repeat("a", 32),
		PID: 1234, Endpoint: "http://10.0.0.1:9000", BearerToken: strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(bad)
	os.WriteFile(p.DaemonDescriptor(), raw, 0o600)
	out, err = cli(t, home, root, "list")
	if err == nil || !strings.Contains(out, "loopback") {
		t.Fatalf("non-loopback descriptor accepted: %s %v", out, err)
	}
}

// TestMalformedClient: every bad input gets a bounded response; the daemon
// stays healthy throughout. Auth is required on every endpoint.
func TestMalformedClient(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "alive-check")
	d := readDescriptor(t, home, root)

	pingOK := func() {
		status, out := rawReq(t, d, http.MethodGet, "/v1/daemon", "", true)
		if status != 200 || !strings.Contains(out, `"apiVersion":1`) {
			t.Fatalf("daemon unhealthy after malformed input: %d %s", status, out)
		}
	}

	// Every endpoint requires the bearer token.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/daemon", ""},
		{http.MethodGet, "/v1/sessions", ""},
		{http.MethodGet, "/v1/sessions/alive-check", ""},
		{http.MethodDelete, "/v1/sessions/alive-check", ""},
		{http.MethodGet, "/v1/sessions/alive-check/transcript", ""},
		{http.MethodGet, "/v1/sessions/alive-check/events", ""},
		{http.MethodPost, "/v1/daemon/shutdown", ""},
		{http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/"}`},
	} {
		status, out := rawReq(t, d, tc.method, tc.path, tc.body, false)
		if status != 401 || !strings.Contains(out, "UNAUTHORIZED") {
			t.Fatalf("%s %s without auth: %d %s", tc.method, tc.path, status, out)
		}
	}
	// Wrong token is also 401.
	req, _ := http.NewRequest(http.MethodGet, d.Endpoint+"/v1/daemon", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("0", 64))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("wrong token: %d", resp.StatusCode)
		}
	}
	pingOK()

	// Malformed JSON body.
	status, out := rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", "{not json", true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("malformed json: %d %s", status, out)
	}
	pingOK()

	// Unknown field / trailing value.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/","exec":"rm"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("unknown field: %d %s", status, out)
	}
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/"} {"key":"y"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("trailing value: %d %s", status, out)
	}
	// Relative cwd.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"rel","cwd":"relative/path"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("relative cwd: %d %s", status, out)
	}
	// Invalid key.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"bad key!","cwd":"/"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_SESSION_KEY") {
		t.Fatalf("bad key: %d %s", status, out)
	}
	pingOK()

	// Oversized body (> 64 KiB).
	big := `{"key":"` + strings.Repeat("A", 70*1024) + `","cwd":"/"}`
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", big, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("oversized: %d %.200s", status, out)
	}
	pingOK()

	// Unknown route / wrong method.
	status, _ = rawReq(t, d, http.MethodGet, "/v1/nope", "", true)
	if status != 404 {
		t.Fatalf("unknown route: %d", status)
	}
	status, out = rawReq(t, d, http.MethodPatch, "/v1/sessions/alive-check", "", true)
	if status != 405 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("method not allowed: %d %s", status, out)
	}
	pingOK()

	// Idle client: connect, send nothing — daemon closes it via the header
	// deadline (never delays shutdown for it either).
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(d.Endpoint, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection was not closed by deadline")
	}
	conn.Close()
	pingOK()

	stopDaemon(t, home, root)
}

// TestDaemonShutdown: graceful stop kills all fixture children and
// removes the descriptor. Durable RelaySessions survive: a later `list`
// autostarts a fresh daemon that restores them COLD (no runtime, no PID).
func TestDaemonShutdown(t *testing.T) {
	home, root := testRoot(t)
	pids := []int{}
	for _, k := range []string{"s1", "s2", "s3"} {
		_, f := serveKey(t, home, root, k)
		pid, _ := strconv.Atoi(f["pid"])
		pids = append(pids, pid)
	}
	dp, _ := daemonStatus(t, home, root)

	mustCLI(t, home, root, "daemon", "stop")
	for _, pid := range pids {
		waitDead(t, pid, 3*time.Second)
	}
	waitDead(t, dp, 3*time.Second)
	desc := descriptorPath(t, home, root)
	if _, err := os.Stat(desc); !os.IsNotExist(err) {
		t.Fatal("descriptor not removed")
	}
	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("daemon status after stop must fail: %s", out)
	}

	// Fresh daemon restores the durable sessions as COLD: all three keys
	// present, no runtime/PID, count=3.
	out := mustCLI(t, home, root, "list")
	for _, k := range []string{"s1", "s2", "s3"} {
		if !strings.Contains(out, "key="+k+" ") {
			t.Fatalf("restored session %s missing: %s", k, out)
		}
	}
	if strings.Count(out, "runtimeState=cold") != 3 || strings.Count(out, " pid=0 ") != 3 {
		t.Fatalf("restored sessions must be COLD: %s", out)
	}
	dp2, count := daemonStatus(t, home, root)
	if dp2 == dp || count != 3 {
		t.Fatalf("fresh daemon pid=%d count=%d (old %d)", dp2, count, dp)
	}
	stopDaemon(t, home, root)
}

// TestForceKillDaemonOrphans: SIGKILL the daemon; fixture children must
// exit via stdin EOF within a bounded time.
func TestForceKillDaemonOrphans(t *testing.T) {
	home, root := testRoot(t)
	_, f := serveKey(t, home, root, "victim")
	fixturePID, _ := strconv.Atoi(f["pid"])
	dp, _ := daemonStatus(t, home, root)

	if err := syscall.Kill(dp, syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon: %v", err)
	}
	waitDead(t, dp, 3*time.Second)
	waitDead(t, fixturePID, 3*time.Second)

	// The stale descriptor is recovered by the next managed command.
	mustCLI(t, home, root, "list")
	stopDaemon(t, home, root)
}

// TestPermissions: private modes on state dirs, descriptor, lock.
func TestPermissions(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "perm")
	defer stopDaemon(t, home, root)

	check := func(path string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Fatalf("%s mode %o want %o", path, got, want)
		}
	}
	check(filepath.Join(root, "relay"), 0o700)
	check(filepath.Join(root, "relay", "run"), 0o700)
	check(filepath.Join(root, "relay", "logs"), 0o700)
	check(filepath.Join(root, "relay", "run", "daemon.json"), 0o600)
	check(filepath.Join(root, "relay", "run", "relayd.lock"), 0o600)
	check(filepath.Join(root, "relay", "sessions"), 0o700)
}

// TestRelativeHomeRejected: relative REPOSUITE_HOME fails before any
// daemon/socket is created.
func TestRelativeHomeRejected(t *testing.T) {
	home, _ := testRoot(t)
	for _, rel := range []string{"reposuite", "./rs", "x/y"} {
		out, err := cli(t, home, rel, "list")
		if err == nil {
			t.Fatalf("REPOSUITE_HOME=%q accepted: %s", rel, out)
		}
		if !strings.Contains(out, "not absolute") {
			t.Fatalf("REPOSUITE_HOME=%q wrong error: %s", rel, out)
		}
		if _, statErr := os.Stat(filepath.Join(home, rel, "relay")); !os.IsNotExist(statErr) {
			t.Fatalf("REPOSUITE_HOME=%q created state", rel)
		}
	}
}

// TestChurn: many create/stop cycles under one daemon — registry returns to
// zero, children are reaped, daemon stays responsive.
func TestChurn(t *testing.T) {
	home, root := testRoot(t)
	dp, _ := serveKey(t, home, root, "seed")
	const n = 60
	var pids []int
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("churn%02d", i)
		dp2, f := serveKey(t, home, root, k)
		if dp2 != dp {
			t.Fatalf("daemon pid changed mid-churn: %d -> %d", dp, dp2)
		}
		pid, _ := strconv.Atoi(f["pid"])
		pids = append(pids, pid)
		mustCLI(t, home, root, "stop", k)
	}
	_, count := daemonStatus(t, home, root)
	if count != 1 { // only the seed remains
		t.Fatalf("sessionCount=%d want 1", count)
	}
	for _, pid := range pids {
		waitDead(t, pid, 2*time.Second)
	}
	stopDaemon(t, home, root)
}

// TestSmokeSequence: the section-48 manual flow end to end.
func TestSmokeSequence(t *testing.T) {
	home, root := testRoot(t)

	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("status before any daemon: %s", out)
	}
	_, a := serveKey(t, home, root, "alpha")
	_, b := serveKey(t, home, root, "beta")
	dp, count := daemonStatus(t, home, root)
	if count != 2 {
		t.Fatalf("sessionCount=%d", count)
	}
	out := mustCLI(t, home, root, "list")
	ai := strings.Index(out, "key=alpha")
	bi := strings.Index(out, "key=beta")
	if ai < 0 || bi < 0 || ai > bi {
		t.Fatalf("list order/content: %s", out)
	}
	sa := mustCLI(t, home, root, "status", "alpha")
	if field(t, sa, "state") != "idle" || field(t, sa, "generation") != "1" ||
		field(t, sa, "runtimeState") != "warm" {
		t.Fatalf("status alpha: %s", sa)
	}
	mustCLI(t, home, root, "stop", "alpha")
	sb := mustCLI(t, home, root, "status", "beta")
	if field(t, sb, "pid") != b["pid"] || field(t, sb, "runtimeId") != b["runtimeId"] {
		t.Fatalf("beta mutated: %s", sb)
	}
	mustCLI(t, home, root, "stop", "beta")
	mustCLI(t, home, root, "daemon", "stop")
	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("daemon status after stop: %s", out)
	}
	_ = a
	_ = dp
}

// TestDaemonPidAndVersion: daemon status exposes pid + API version.
func TestDaemonPidAndVersion(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "x")
	out := mustCLI(t, home, root, "daemon", "status")
	pid, _ := strconv.Atoi(field(t, out, "pid"))
	if pid <= 1 || !alive(pid) {
		t.Fatalf("daemon pid %d not alive", pid)
	}
	if v := field(t, out, "apiVersion"); v != "1" {
		t.Fatalf("apiVersion=%s", v)
	}
	stopDaemon(t, home, root)
}

// TestBadRequestShapes: structurally valid JSON, semantically bad —
// stable machine codes on both path and body key validation.
func TestBadRequestShapes(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "ok")
	d := readDescriptor(t, home, root)

	status, out := rawReq(t, d, http.MethodGet, "/v1/sessions/missing", "", true)
	if status != 404 || !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("missing: %d %s", status, out)
	}
	status, out = rawReq(t, d, http.MethodDelete, "/v1/sessions/missing", "", true)
	if status != 404 || !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("stop missing: %d %s", status, out)
	}
	// Path-based key validation (single segment, URL-escaped space).
	status, out = rawReq(t, d, http.MethodGet, "/v1/sessions/bad%20key", "", true)
	if status != 400 || !strings.Contains(out, "INVALID_SESSION_KEY") {
		t.Fatalf("bad path key: %d %s", status, out)
	}
	// Path traversal is never a valid session route.
	status, _ = rawReq(t, d, http.MethodGet, "/v1/sessions/..%2Fx", "", true)
	if status != 404 {
		t.Fatalf("traversal route: %d", status)
	}
	// Duplicate create is a conflict.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"ok","cwd":"/"}`, true)
	if status != 409 || !strings.Contains(out, "SESSION_EXISTS") {
		t.Fatalf("duplicate: %d %s", status, out)
	}
	stopDaemon(t, home, root)
}
