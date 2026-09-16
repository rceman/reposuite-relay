package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is re-exec'd as a controlled harness child. The
// RUNTIME_HELPER env var selects behavior; without it the test is a no-op.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("RUNTIME_HELPER") {
	case "":
		return
	case "eof": // cooperative: block on stdin, exit 0 on EOF
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
	case "spawnchild": // spawns a grandchild, reports its pid, blocks on stdin
		child := exec.Command("sleep", "600")
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		_, _ = os.Stdout.WriteString(fmt.Sprint(child.Process.Pid) + "\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

// procAlive reports whether pid exists (kill 0 semantics).
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// spawnHelper re-execs this test binary as a controlled child.
func spawnHelper(t *testing.T, mode string) *Process {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "RUNTIME_HELPER="+mode)
	cmd.Dir = t.TempDir()
	p, err := Adopt(cmd)
	if err != nil {
		t.Fatal(err)
	}
	// The kill-fallback timer only bounds the wait; normal children exit
	// on their own before it matters. A re-exec'd test binary can take
	// ~1s just to boot under -race, so only the stubborn child (which
	// never exits on EOF) gets a short fallback timer.
	if mode == "stubborn" {
		p.gracefulTimeout = 300 * time.Millisecond
	}
	p.killTimeout = 3 * time.Second
	return p
}

// assertReaped proves cmd.Wait() completed — ProcessState is only set
// after the process terminated AND was reaped.
func assertReaped(t *testing.T, p *Process) {
	t.Helper()
	if p.cmd.ProcessState == nil {
		t.Fatalf("pid=%d not reaped (ProcessState unset)", p.PID())
	}
}

// TestStopNormalEOF: child exits 0 after stdin EOF → Stop nil.
func TestStopNormalEOF(t *testing.T) {
	p := spawnHelper(t, "eof")
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertReaped(t, p)
	if p.cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %d", p.cmd.ProcessState.ExitCode())
	}
	if !p.Exited() {
		t.Fatal("Exited() false after confirmed stop")
	}
}

// TestStopNonZeroExit: a child that already exited non-zero — termination
// is confirmed, so Stop reports success and the process is reaped.
func TestStopNonZeroExit(t *testing.T) {
	p := spawnHelper(t, "exit3")
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop must succeed for confirmed termination, got %v", err)
	}
	assertReaped(t, p)
	if p.cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", p.cmd.ProcessState.ExitCode())
	}
}

// TestStopKillErrProcessDoneRace: the graceful signal reports
// ErrProcessDone (child died concurrently with the fallback) — the
// bounded reap still confirms termination, so Stop returns nil and never
// hangs.
func TestStopKillErrProcessDoneRace(t *testing.T) {
	p := spawnHelper(t, "delayedexit")
	p.gracefulTimeout = 150 * time.Millisecond // expire before child's 500ms exit
	p.killTimeout = 3 * time.Second
	p.signal = func(int, syscall.Signal) error { return os.ErrProcessDone } // forced race seam
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertReaped(t, p)
	// Stop must have waited for the real reap (~500ms), not returned early.
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("Stop took %s — reap was not bounded-confirmed", elapsed)
	}
}

// TestStopKillFailureUnconfirmed: a genuine signal error with no reap
// confirmation within the bound → explicit error, not a hang and not a
// silent success.
func TestStopKillFailureUnconfirmed(t *testing.T) {
	p := spawnHelper(t, "stubborn")
	p.gracefulTimeout = 150 * time.Millisecond
	p.termTimeout = 150 * time.Millisecond
	p.killTimeout = 200 * time.Millisecond
	p.signal = func(int, syscall.Signal) error { return os.ErrPermission } // e.g. EPERM
	start := time.Now()
	err := p.Stop()
	if err == nil {
		t.Fatal("Stop must fail when termination cannot be confirmed")
	}
	if !errors.Is(err, ErrNotReaped) {
		t.Fatalf("want ErrNotReaped, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop hung %s past the bounded waits", elapsed)
	}
	// Cleanup the still-running child for real, then wait for the reap.
	_ = p.cmd.Process.Kill()
	select {
	case <-p.Wait():
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup kill not reaped")
	}
	assertReaped(t, p)
}

// TestStopForcedKill: a child that ignores stdin EOF hits the bounded
// TERM/KILL fallback; confirmed reaping → Stop nil.
func TestStopForcedKill(t *testing.T) {
	p := spawnHelper(t, "stubborn")
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop after SIGKILL must be nil, got %v", err)
	}
	assertReaped(t, p)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("stubborn child exited before the kill fallback — test inconclusive")
	}
	// Signal death: Exited() is false for a normal exit and ExitCode()
	// reports -1.
	if p.cmd.ProcessState.ExitCode() >= 0 {
		t.Fatalf("stubborn child exit code = %d, want signal death", p.cmd.ProcessState.ExitCode())
	}
}

// TestProcessGroupReap: a child that spawns its own child (models the
// Codex node subprocess) is torn down as a whole tree — no orphan
// survives the runtime stop.
func TestProcessGroupReap(t *testing.T) {
	// The helper spawns `sleep 600` as a grandchild, prints its pid, then
	// blocks on stdin forever.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "RUNTIME_HELPER=spawnchild")
	cmd.Dir = t.TempDir()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = pw
	p, err := Adopt(cmd)
	if err != nil {
		t.Fatal(err)
	}
	pw.Close()
	_ = pr.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := pr.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("grandchild pid not reported: %v", err)
	}
	var grandchild int
	if _, err := fmt.Sscan(string(buf[:n]), &grandchild); err != nil || grandchild <= 0 {
		t.Fatalf("bad grandchild pid %q", buf[:n])
	}
	if !procAlive(grandchild) {
		t.Fatal("grandchild not running")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("stop tree: %v", err)
	}
	// The whole group must be gone: SIGKILL reaches the grandchild too.
	deadline := time.Now().Add(3 * time.Second)
	for procAlive(grandchild) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(grandchild, syscall.SIGKILL)
			t.Fatalf("grandchild pid %d survived runtime teardown", grandchild)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
