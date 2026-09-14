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
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// StopTimeout bounds how long a session stop waits for the child to exit
// after its stdin pipe is closed before SIGKILL.
const StopTimeout = 3 * time.Second

// Child is a daemon-owned fixture process.
type Child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wait  chan error
}

// Spawn starts `selfExe __fixture` in cwd with a daemon-owned stdin pipe.
// The child is started detached from the daemon's controlling-tty signals:
// it is in the daemon's session, and its stdin is our pipe.
func Spawn(selfExe, cwd string) (*Child, error) {
	cmd := exec.Command(selfExe, "__fixture")
	cmd.Dir = cwd
	cmd.Stdout = nil // /dev/null
	cmd.Stderr = nil // /dev/null
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fixture start: %w", err)
	}
	c := &Child{cmd: cmd, stdin: stdin, wait: make(chan error, 1)}
	go func() { c.wait <- cmd.Wait() }()
	return c, nil
}

// PID is the fixture child's process id.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Stop closes the child's stdin (its exit signal), waits up to StopTimeout,
// then SIGKILLs as a bounded fallback.
func (c *Child) Stop() error {
	_ = c.stdin.Close()
	select {
	case err := <-c.wait:
		return err
	case <-time.After(StopTimeout):
		_ = c.cmd.Process.Kill()
		return <-c.wait
	}
}

// RunChild is the `__fixture` child entry point: block on stdin until EOF,
// then exit 0. EOF arrives when the daemon closes the pipe (stop) or dies
// (kernel cleanup), so the fixture can never be orphaned.
func RunChild() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
