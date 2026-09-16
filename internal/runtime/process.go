// Package runtime implements the RuntimeSupervisor: the daemon's
// ephemeral authority over harness processes.
//
// Ownership model (ADR-005): RelaySession is durable logical identity and
// lives in internal/session + internal/store. HarnessRuntime is an
// ephemeral process generation owned here and never persisted. One
// runtime may host several sessions (a shared Codex app-server hosts many
// native threads), so deleting one session must never kill a runtime
// another session still uses.
//
// This package owns: runtime identity/PID/start time, session<->runtime
// bindings, process stop/reap (whole process tree), and per-session
// activity/blocker state used for sleep authority. It is deliberately a
// small explicit component — not a service container.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Bounded stop phases. Every wait is bounded; there is no unbounded Wait
// anywhere in the stop path.
const (
	// GracefulTimeout bounds the wait after the harness's stdin is closed
	// (the cooperative shutdown signal).
	GracefulTimeout = 3 * time.Second
	// TermTimeout bounds the wait after SIGTERM to the process group.
	TermTimeout = 3 * time.Second
	// KillTimeout bounds the wait after SIGKILL to the process group.
	KillTimeout = 3 * time.Second
)

// ErrNotReaped means termination could not be confirmed within the
// bounded waits.
var ErrNotReaped = errors.New("process not reaped within bounds")

// Spec describes one harness process to spawn. Exec is always direct
// (never a shell) and the child always gets its own process group.
type Spec struct {
	Path string   // absolute or PATH-resolved executable
	Args []string // arguments (never shell-interpreted)
	Dir  string   // working directory
	Env  []string // nil inherits the daemon environment

	// StdinPipe exposes a writable stdin pipe. Closing it is the
	// cooperative shutdown signal every harness must honor.
	StdinPipe bool
	// Stdout/Stderr receive the child's streams; nil discards them.
	Stdout io.Writer
	Stderr io.Writer
}

// Process is one owned harness process tree: direct exec, its own
// process group (so a runtime's node/subprocess children die with it),
// and a bounded, confirmed stop path.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	doneCh chan struct{} // closed when the process is terminated AND reaped

	mu  sync.Mutex
	err error

	// Test seams: bounded phase timers and the group-signal primitive.
	gracefulTimeout time.Duration
	termTimeout     time.Duration
	killTimeout     time.Duration
	signal          func(pid int, sig syscall.Signal) error
}

// Spawn starts a new process for spec.
func Spawn(spec Spec) (*Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	return start(cmd, spec.StdinPipe, spec.Stdout, spec.Stderr)
}

// Adopt starts an already-prepared *exec.Cmd (test seam for controlled
// stubborn/helper processes). The caller must not have started it.
func Adopt(cmd *exec.Cmd) (*Process, error) {
	return start(cmd, true, nil, nil)
}

func start(cmd *exec.Cmd, stdinPipe bool, stdout, stderr io.Writer) (*Process, error) {
	// Own process group: the harness and everything it spawns (e.g. a
	// runtime's child node process) is one killable, reapable unit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// nil means "discard" for Spawn; Adopt preserves streams the caller
	// already wired on the prepared command.
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	p := &Process{
		cmd:             cmd,
		doneCh:          make(chan struct{}),
		gracefulTimeout: GracefulTimeout,
		termTimeout:     TermTimeout,
		killTimeout:     KillTimeout,
	}
	p.signal = func(pid int, sig syscall.Signal) error {
		return syscall.Kill(-pid, sig) // whole group
	}
	if stdinPipe {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin pipe: %w", err)
		}
		p.stdin = in
	}
	if err := cmd.Start(); err != nil {
		if p.stdin != nil {
			p.stdin.Close()
		}
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.doneCh)
	}()
	return p, nil
}

// PID is the process-group leader's id (0 before a successful start).
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Stdin returns the writable stdin pipe (nil when the spec had none).
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Wait returns a channel closed once the process is terminated AND
// reaped (cmd.Wait completed). Safe for any number of waiters; never
// blocks.
func (p *Process) Wait() <-chan struct{} { return p.doneCh }

// Exited reports whether the process is reaped.
func (p *Process) Exited() bool {
	select {
	case <-p.doneCh:
		return true
	default:
		return false
	}
}

// Err returns the exit error (nil while running; a non-nil *exec.ExitError
// after termination means the process exited non-zero or by signal).
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stop terminates the whole process tree with bounded phases and confirms
// reaping:
//
//  1. close stdin (cooperative shutdown; EOF)
//  2. wait up to gracefulTimeout
//  3. SIGTERM to the process group, wait up to termTimeout
//  4. SIGKILL to the process group, wait up to killTimeout
//
// Stop success means the runtime tree is CONFIRMED gone — regardless of
// exit status (exit 0, non-zero, signal). A non-zero *exec.ExitError is a
// successful stop, not a lifecycle failure.
func (p *Process) Stop() error {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if err := p.waitReaped(p.gracefulTimeout); err == nil {
		// The leader exited cooperatively, but the tree contract is
		// "complete runtime process tree gone": sweep any survivors.
		p.sweepGroup()
		return nil
	}
	pid := p.PID()
	if pid > 0 {
		_ = p.signal(pid, syscall.SIGTERM)
	}
	if err := p.waitReaped(p.termTimeout); err == nil {
		p.sweepGroup()
		return nil
	}
	if pid > 0 {
		_ = p.signal(pid, syscall.SIGKILL)
	}
	if err := p.waitReaped(p.killTimeout); err == nil {
		return nil
	}
	return fmt.Errorf("process pid=%d: %w", pid, ErrNotReaped)
}

// sweepGroup kills any remaining members of the leader's process group
// and waits (bounded) for the group to disappear. A leader that exited
// gracefully may have left children (a runtime's node subprocess) that
// must not outlive the runtime.
func (p *Process) sweepGroup() {
	pid := p.PID()
	if pid <= 0 {
		return
	}
	if err := p.signal(pid, 0); err != nil {
		return // no process left in the group (ESRCH)
	}
	_ = p.signal(pid, syscall.SIGKILL)
	deadline := time.Now().Add(p.killTimeout)
	for time.Now().Before(deadline) {
		if err := p.signal(pid, 0); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitReaped waits up to timeout for cmd.Wait to complete.
func (p *Process) waitReaped(timeout time.Duration) error {
	select {
	case <-p.doneCh:
		return confirmed(p.Err())
	case <-time.After(timeout):
		return fmt.Errorf("process pid=%d: %w after %s", p.PID(), ErrNotReaped, timeout)
	}
}

// confirmed turns a cmd.Wait result into lifecycle truth: an ExitError
// means the process exited (non-zero status or signal) — termination is
// confirmed, so Stop succeeded.
func confirmed(err error) error {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return fmt.Errorf("wait: %w", err)
	}
	return nil
}
