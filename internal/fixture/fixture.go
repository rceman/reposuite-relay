// Package fixture implements the deterministic fixture harness used to
// prove daemon/session ownership before real harnesses exist.
//
// The fixture child is the SAME executable in the hidden `__fixture` mode.
// It blocks reading a daemon-owned stdin pipe until EOF, so it always exits
// when the daemon stops it — and, crucially, when the daemon dies for any
// reason (the kernel closes the pipe's write end). Fixture processes can
// therefore never outlive their daemon.
//
// It executes no client-supplied command, needs no PTY, and needs no
// terminal model.
package fixture

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// StopTimeout bounds the graceful wait after the child's stdin pipe is
// closed before SIGKILL; KillTimeout bounds the reap wait after SIGKILL.
// Neither wait is unbounded.
const (
	StopTimeout = 3 * time.Second
	KillTimeout = 3 * time.Second
)

// Child is a daemon-owned fixture process.
type Child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wait  chan error

	stopTimeout time.Duration
	killTimeout time.Duration
}

// Spawn starts `selfExe __fixture` in cwd with a daemon-owned stdin pipe.
// The child is started detached from the daemon's controlling-tty signals:
// it is in the daemon's session, and its stdin is our pipe.
func Spawn(selfExe, cwd string) (*Child, error) {
	cmd := exec.Command(selfExe, "__fixture")
	cmd.Dir = cwd
	return SpawnCmd(cmd)
}

// SpawnCmd starts an already-prepared *exec.Cmd as a fixture child — the
// seam tests use to prove forced-kill semantics with a controlled stubborn
// process. cmd's StdinPipe is taken over; Dir/Env are the caller's.
func SpawnCmd(cmd *exec.Cmd) (*Child, error) {
	cmd.Stdout = nil // /dev/null
	cmd.Stderr = nil // /dev/null
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fixture start: %w", err)
	}
	c := &Child{
		cmd:         cmd,
		stdin:       stdin,
		wait:        make(chan error, 1),
		stopTimeout: StopTimeout,
		killTimeout: KillTimeout,
	}
	go func() { c.wait <- cmd.Wait() }()
	return c, nil
}

// PID is the fixture child's process id.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Stop closes the child's stdin (its exit signal), waits up to stopTimeout,
// then SIGKILLs and waits up to killTimeout for reaping.
//
// Stop success means the process is CONFIRMED terminated and reaped —
// regardless of exit status (exit 0, non-zero, signal, SIGKILL). It only
// returns an error when termination/reaping cannot be confirmed, e.g. the
// kill fails while the child is still un-reaped. A non-zero
// *exec.ExitError is therefore a successful stop, not a lifecycle failure.
func (c *Child) Stop() error {
	_ = c.stdin.Close()
	select {
	case err := <-c.wait:
		return confirmed(err)
	case <-time.After(c.stopTimeout):
	}
	if err := c.cmd.Process.Kill(); err != nil {
		// Kill failed — if the process already exited on its own, wait
		// confirms it; otherwise we cannot confirm termination.
		if errors.Is(err, os.ErrProcessDone) {
			return confirmed(<-c.wait)
		}
		return fmt.Errorf("fixture kill pid=%d: %w", c.PID(), err)
	}
	select {
	case err := <-c.wait:
		return confirmed(err)
	case <-time.After(c.killTimeout):
		return fmt.Errorf("fixture pid=%d did not reap after SIGKILL", c.PID())
	}
}

// confirmed turns a cmd.Wait result into lifecycle truth: an ExitError
// means the process exited (non-zero status or signal) — termination is
// confirmed, so Stop succeeded. Only a non-ExitError wait failure is real.
func confirmed(err error) error {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return fmt.Errorf("fixture wait: %w", err)
	}
	return nil
}

// RunChild is the `__fixture` child entry point: block on stdin until EOF,
// then exit 0. EOF arrives when the daemon closes the pipe (stop) or dies
// (kernel cleanup), so the fixture can never be orphaned.
func RunChild() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
