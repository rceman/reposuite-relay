package daemon_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestDaemonAbsentByDefault: no autostart for status/stop. Stop on an
// absent daemon is idempotent success (canonical lifecycle contract), but
// it must never START one.
func TestDaemonAbsentByDefault(t *testing.T) {
	home, root := testRoot(t)
	daemonStopped(t, home, root)
	out, err := cli(t, home, root, "daemon", "stop")
	if err != nil || !strings.Contains(out, "already stopped") {
		t.Fatalf("daemon stop on empty root: %v: %s", err, out)
	}
	daemonStopped(t, home, root) // still stopped — nothing was spawned
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
	if out, err := cli(t, home, root, "session", "status", "alpha"); err == nil {
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
	sb := mustCLI(t, home, root, "session", "status", "beta")
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
	sb := mustCLI(t, home, root, "session", "status", "same")
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
	daemonStopped(t, home, root)

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
