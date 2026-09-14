package daemon_test

// End-to-end tests for the daemon/session milestone. Every test uses an
// isolated temporary REPOSUITE_HOME + HOME; nothing touches real user
// state, ~/.airelay, or live processes outside the PIDs the test created.

import (
	"encoding/json"
	"fmt"
	"net"
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

	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/protocol"
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
		"pid":        field(t, out, "pid"),
		"runtimeId":  field(t, out, "runtimeId"),
		"generation": field(t, out, "generation"),
		"state":      field(t, out, "state"),
		"harness":    field(t, out, "harness"),
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
	sock := filepath.Join(root, "relay", "run", "relayd.sock")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("socket still present after daemon stop")
}

func socketPath(t *testing.T, home, root string) string {
	t.Helper()
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	return p.DaemonSocket()
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

// TestStaleSocketRecovery: a stale Unix socket inode is reclaimed by the
// daemon lock owner.
func TestStaleSocketRecovery(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSocket())
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // keep the inode: dead socket
	ln.Close()                                     // leaves a stale socket behind
	if _, err := os.Lstat(p.DaemonSocket()); err != nil {
		t.Fatalf("stale socket not created: %v", err)
	}
	// A managed command must recover and start the daemon.
	mustCLI(t, home, root, "list")
	daemonStatus(t, home, root)
	stopDaemon(t, home, root)
}

// TestNonSocketCollision: a regular file at relayd.sock is never deleted;
// startup fails closed.
func TestNonSocketCollision(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	marker := []byte("do not delete me")
	if err := os.WriteFile(p.DaemonSocket(), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, home, root, "list")
	if err == nil {
		t.Fatalf("daemon start must fail on non-socket collision: %s", out)
	}
	got, _ := os.ReadFile(p.DaemonSocket())
	if string(got) != string(marker) {
		t.Fatal("collision file was modified/deleted")
	}
}

// TestMalformedClient: every bad input gets a bounded response; the daemon
// stays healthy throughout.
func TestMalformedClient(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "alive-check")
	sock := socketPath(t, home, root)

	raw := func(t *testing.T, payload []byte) (string, error) {
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			return "", err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(6 * time.Second))
		if _, err := c.Write(payload); err != nil {
			return "", err
		}
		if uc, ok := c.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
		var b strings.Builder
		buf := make([]byte, 8192)
		for {
			n, err := c.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return b.String(), nil
	}

	pingOK := func() {
		out, err := raw(t, []byte(`{"v":1,"op":"ping"}`))
		if err != nil || !strings.Contains(out, `"ok":true`) {
			t.Fatalf("daemon unhealthy after malformed input: %v %s", err, out)
		}
	}

	out, _ := raw(t, []byte("{not json"))
	if !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("malformed json: %s", out)
	}
	pingOK()

	out, _ = raw(t, []byte(`{"v":1,"op":"nope"}`))
	if !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("unknown op: %s", out)
	}
	pingOK()

	out, _ = raw(t, []byte(`{"v":2,"op":"ping"}`))
	if !strings.Contains(out, "PROTOCOL_MISMATCH") {
		t.Fatalf("bad version: %s", out)
	}
	pingOK()

	// Oversized request.
	big := append([]byte(`{"v":1,"op":"x","key":"`), []byte(strings.Repeat("A", 70*1024))...)
	big = append(big, []byte(`"}`)...)
	out, _ = raw(t, big)
	if !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("oversized: %s", out[:min(200, len(out))])
	}
	pingOK()

	// Idle client: connect, send nothing — daemon must close it via deadline.
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(8 * time.Second))
	_, err = c.Read(make([]byte, 1))
	c.Close()
	if err == nil {
		t.Fatal("idle connection was not closed by deadline")
	}
	pingOK()

	stopDaemon(t, home, root)
}

// TestDaemonShutdown: graceful stop kills all fixture children, removes
// socket, and a later `list` autostarts a fresh empty daemon.
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
	sock := socketPath(t, home, root)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
	if out, err := cli(t, home, root, "daemon", "status"); err == nil {
		t.Fatalf("daemon status after stop must fail: %s", out)
	}

	// Fresh daemon: empty registry (no persistence).
	out := mustCLI(t, home, root, "list")
	if strings.Contains(out, "key=") {
		t.Fatalf("fresh daemon must be empty: %s", out)
	}
	dp2, count := daemonStatus(t, home, root)
	if dp2 == dp || count != 0 {
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

	// The stale socket is recovered by the next managed command.
	mustCLI(t, home, root, "list")
	stopDaemon(t, home, root)
}

// TestPermissions: private modes on state dirs, socket, lock.
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
	check(filepath.Join(root, "relay", "run", "relayd.sock"), 0o600)
	check(filepath.Join(root, "relay", "run", "relayd.lock"), 0o600)
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
	if field(t, sa, "state") != "running" || field(t, sa, "generation") != "1" {
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

// TestDaemonPidAndVersion: ping exposes pid + protocol version.
func TestDaemonPidAndVersion(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "x")
	out := mustCLI(t, home, root, "daemon", "status")
	pid, _ := strconv.Atoi(field(t, out, "pid"))
	if pid <= 1 || !alive(pid) {
		t.Fatalf("daemon pid %d not alive", pid)
	}
	if v := field(t, out, "protocolVersion"); v != "1" {
		t.Fatalf("protocolVersion=%s", v)
	}
	stopDaemon(t, home, root)
}

// TestBadProtocolRequestShapes: structurally valid JSON, semantically bad.
func TestBadProtocolRequestShapes(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "ok")
	sock := socketPath(t, home, root)

	do := func(req string) protocol.Response {
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(6 * time.Second))
		fmt.Fprintln(c, req)
		if uc, ok := c.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
		var resp protocol.Response
		if err := json.NewDecoder(c).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := do(`{"v":1,"op":"serve_fixture","key":"bad key!"}`)
	if r.OK || r.Code != protocol.ErrInvalidSessionKey {
		t.Fatalf("bad key: %+v", r)
	}
	r = do(`{"v":1,"op":"session_status","key":"missing"}`)
	if r.OK || r.Code != protocol.ErrSessionNotFound {
		t.Fatalf("missing: %+v", r)
	}
	r = do(`{"v":1,"op":"stop_session","key":"missing"}`)
	if r.OK || r.Code != protocol.ErrSessionNotFound {
		t.Fatalf("stop missing: %+v", r)
	}
	stopDaemon(t, home, root)
}
