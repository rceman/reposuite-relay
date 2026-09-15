package fixture

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestHelperProcess is re-exec'd as a controlled fixture child. The
// FIXTURE_HELPER env var selects behavior; without it the test is a no-op.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("FIXTURE_HELPER") {
	case "":
		return
	case "eof": // normal fixture behavior: block on stdin, exit 0 on EOF
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "exit3": // exits non-zero immediately, ignoring stdin
		os.Exit(3)
	case "stubborn": // never reads stdin, never exits on its own
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	case "delayedexit": // ignores stdin, exits 0 after a fixed delay —
		// models a child dying concurrently with the kill fallback
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}
}

// spawnHelper re-execs this test binary as a fixture child.
func spawnHelper(t *testing.T, mode string) *Child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "FIXTURE_HELPER="+mode)
	cmd.Dir = t.TempDir()
	c, err := SpawnCmd(cmd)
	if err != nil {
		t.Fatal(err)
	}
	// The kill-fallback timer only bounds the wait; normal children exit
	// on their own before it matters. A re-exec'd test binary can take
	// ~1s just to boot under -race, so only the stubborn child (which
	// never exits on EOF) gets a short fallback timer.
	if mode == "stubborn" {
		c.stopTimeout = 300 * time.Millisecond
	}
	c.killTimeout = 3 * time.Second
	return c
}

// assertReaped proves cmd.Wait() completed — ProcessState is only set
// after the process terminated AND was reaped.
func assertReaped(t *testing.T, c *Child) {
	t.Helper()
	if c.cmd.ProcessState == nil {
		t.Fatalf("pid=%d not reaped (ProcessState unset)", c.PID())
	}
}

// TestStopNormalEOF: child exits 0 after stdin EOF → Stop nil.
func TestStopNormalEOF(t *testing.T) {
	c := spawnHelper(t, "eof")
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertReaped(t, c)
	if c.cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %d", c.cmd.ProcessState.ExitCode())
	}
}

// TestStopNonZeroExit: a child that already exited non-zero — termination
// is confirmed, so Stop reports success and the process is reaped.
func TestStopNonZeroExit(t *testing.T) {
	c := spawnHelper(t, "exit3")
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop must succeed for confirmed termination, got %v", err)
	}
	assertReaped(t, c)
	if c.cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", c.cmd.ProcessState.ExitCode())
	}
}

// TestStopKillErrProcessDoneRace: Kill reports ErrProcessDone (child died
// concurrently with the fallback) — the bounded reap still confirms
// termination, so Stop returns nil and never hangs.
func TestStopKillErrProcessDoneRace(t *testing.T) {
	c := spawnHelper(t, "delayedexit")
	c.stopTimeout = 150 * time.Millisecond // expire before child's 500ms exit
	c.killTimeout = 3 * time.Second
	c.kill = func() error { return os.ErrProcessDone } // forced race seam
	start := time.Now()
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertReaped(t, c)
	// Stop must have waited for the real reap (~500ms), not returned early.
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("Stop took %s — reap was not bounded-confirmed", elapsed)
	}
}

// TestStopKillFailureUnconfirmed: a genuine kill error with no reap
// confirmation within the bound → explicit error, not a hang and not a
// silent success.
func TestStopKillFailureUnconfirmed(t *testing.T) {
	c := spawnHelper(t, "stubborn")
	c.stopTimeout = 150 * time.Millisecond
	c.killTimeout = 200 * time.Millisecond
	c.kill = func() error { return os.ErrPermission } // e.g. EPERM
	start := time.Now()
	err := c.Stop()
	if err == nil {
		t.Fatal("Stop must fail when termination cannot be confirmed")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop hung %s past the bounded waits", elapsed)
	}
	// Cleanup the still-running child for real, then wait for the reap.
	// Receiving c.wait synchronizes with the Wait goroutine, making the
	// ProcessState read below race-free.
	_ = c.cmd.Process.Kill()
	select {
	case <-c.wait:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup kill not reaped")
	}
	assertReaped(t, c)
}

// TestStopForcedKill: a child that ignores stdin EOF hits the bounded
// SIGKILL fallback; confirmed reaping → Stop nil.
func TestStopForcedKill(t *testing.T) {
	c := spawnHelper(t, "stubborn")
	start := time.Now()
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop after SIGKILL must be nil, got %v", err)
	}
	assertReaped(t, c)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("stubborn child exited before the kill fallback — test inconclusive")
	}
	// SIGKILL: terminated by signal, not a normal exit — Exited() is false
	// for signal death and ExitCode() reports -1.
	if c.cmd.ProcessState.ExitCode() >= 0 {
		t.Fatalf("stubborn child exit code = %d, want signal death", c.cmd.ProcessState.ExitCode())
	}
}
