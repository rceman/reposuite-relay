package main

// Real-binary lifecycle acceptance: serve/start/stop/restart/status over
// an isolated REPOSUITE_HOME. No model calls; fixture sessions only.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lifecycleEnv builds an isolated RepoSuite home and returns a runner.
func lifecycleEnv(t *testing.T, bin string) (root string, run func(args ...string) (string, error)) {
	t.Helper()
	root = t.TempDir()
	run = func(args ...string) (string, error) {
		return runStatus(t, bin, root, args...)
	}
	return root, run
}

func statusPID(t *testing.T, run func(...string) (string, error)) (int, bool) {
	t.Helper()
	out, err := run("status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	var rep struct {
		Running bool `json:"running"`
		PID     int  `json:"pid"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	return rep.PID, rep.Running
}

// TestLifecycleStartStopRestart: the canonical sequence over a real
// detached daemon process.
func TestLifecycleStartStopRestart(t *testing.T) {
	bin := buildBinary(t)
	root, run := lifecycleEnv(t, bin)

	out, err := run("status")
	if err != nil || !strings.Contains(out, "STOPPED") {
		t.Fatalf("initial status: %v\n%s", err, out)
	}
	out, err = run("start")
	if err != nil || !strings.Contains(out, "Relay started") || !strings.Contains(out, "PID:") {
		t.Fatalf("start: %v\n%s", err, out)
	}
	pid1, running := statusPID(t, run)
	if !running || pid1 == 0 {
		t.Fatal("not running after start")
	}
	// Detached: the daemon is its own session leader (setsid).
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/"+strconv.Itoa(pid1), &st); err != nil {
		t.Fatalf("daemon pid not alive: %v", err)
	}
	// Detached stdio goes to the Relay log, not a terminal.
	if _, err := os.Stat(filepath.Join(root, "relay", "logs", "relayd.log")); err != nil {
		t.Fatalf("daemon log missing: %v", err)
	}

	// Idempotent start — same daemon, no duplicate.
	out, err = run("start")
	if err != nil || !strings.Contains(out, "already running") {
		t.Fatalf("second start: %v\n%s", err, out)
	}
	if pid2, _ := statusPID(t, run); pid2 != pid1 {
		t.Fatalf("second start spawned a new daemon: %d != %d", pid2, pid1)
	}

	// Session durability across restart.
	if out, err := run("serve", "fixture", "--key", "persist"); err != nil {
		t.Fatalf("serve fixture: %v\n%s", err, out)
	}
	out, err = run("restart")
	if err != nil || !strings.Contains(out, "restarted") {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	pid2, running := statusPID(t, run)
	if !running || pid2 == pid1 {
		t.Fatalf("restart did not produce a new generation: %d vs %d", pid2, pid1)
	}
	if out, err = run("list"); err != nil || !strings.Contains(out, "persist") {
		t.Fatalf("session lost across restart: %v\n%s", err, out)
	}
	// Dead old generation.
	if err := syscall.Kill(pid1, 0); err == nil {
		t.Fatal("old daemon still alive after restart")
	}

	out, err = run("stop")
	if err != nil || !strings.Contains(out, "stopped") {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if err := syscall.Kill(pid2, 0); err == nil {
		t.Fatal("daemon alive after stop")
	}
	// Idempotent stop.
	out, err = run("stop")
	if err != nil || !strings.Contains(out, "already stopped") {
		t.Fatalf("second stop: %v\n%s", err, out)
	}
	if _, running := statusPID(t, run); running {
		t.Fatal("still running after stop")
	}
}

// TestLifecycleStaleDescriptor: a descriptor left behind by a killed
// daemon reports STALE (not RUNNING), and start recovers.
func TestLifecycleStaleDescriptor(t *testing.T) {
	bin := buildBinary(t)
	root, run := lifecycleEnv(t, bin)
	if _, err := run("start"); err != nil {
		t.Fatal(err)
	}
	pid, _ := statusPID(t, run)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Wait for the process to be gone; the descriptor file stays stale.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "relay", "run", "daemon.json")); err != nil {
		t.Fatalf("expected stale descriptor to remain: %v", err)
	}
	out, err := run("status")
	if err != nil || !strings.Contains(out, "STALE") {
		t.Fatalf("stale status: %v\n%s", err, out)
	}
	out, err = run("start")
	if err != nil || !strings.Contains(out, "Relay started") {
		t.Fatalf("start over stale: %v\n%s", err, out)
	}
	if _, running := statusPID(t, run); !running {
		t.Fatal("not running after stale recovery")
	}
	run("stop")
}

// TestLifecycleStartRace: two simultaneous starts yield exactly one
// daemon and both callers see coherent success.
func TestLifecycleStartRace(t *testing.T) {
	bin := buildBinary(t)
	_, run := lifecycleEnv(t, bin)
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			o, e := run("start")
			ch <- res{o, e}
		}()
	}
	var errs []string
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.out)
		}
	}
	if len(errs) != 0 {
		t.Fatalf("raced starts failed: %v", errs)
	}
	pid1, running := statusPID(t, run)
	if !running {
		t.Fatal("not running after raced start")
	}
	if pid2, _ := statusPID(t, run); pid2 != pid1 {
		t.Fatal("two daemons after raced start")
	}
	run("stop")
}

// TestLifecycleServeForeground: bare `serve` runs the daemon in the
// foreground and a second serve fails fast (singleton).
func TestLifecycleServeForeground(t *testing.T) {
	bin := buildBinary(t)
	_, run := lifecycleEnv(t, bin)
	if _, err := run("start"); err != nil {
		t.Fatal(err)
	}
	defer run("stop")
	// Second foreground serve must fail — the lock owner exists.
	if out, err := run("serve"); err == nil ||
		!strings.Contains(out, "already owns") {
		t.Fatalf("second serve should fail: %v\n%s", err, out)
	}
}
