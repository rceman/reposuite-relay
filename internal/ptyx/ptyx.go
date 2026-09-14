// Package ptyx is a minimal Linux PTY runner for the spike: a child process
// on a creack/pty master/slave pair whose output is pumped into a consumer
// (typically the headless terminal model) and which accepts input writes.
package ptyx

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// Process is a child attached to a PTY.
type Process struct {
	Cmd  *exec.Cmd
	Ptmx *os.File

	mu      sync.Mutex
	closed  bool
	waitErr error
	waitCh  chan struct{}
}

// Start spawns name/args on a fresh PTY sized cols x rows.
func Start(name string, args []string, cols, rows uint16, env []string, dir string) (*Process, error) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	if dir != "" {
		cmd.Dir = dir
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	p := &Process{Cmd: cmd, Ptmx: ptmx, waitCh: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitCh)
	}()
	return p, nil
}

// Resize changes the PTY window size (TIOCSWINSZ; delivers SIGWINCH).
func (p *Process) Resize(cols, rows uint16) error {
	return pty.Setsize(p.Ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Size reports the current PTY window size.
func (p *Process) Size() (cols, rows int, err error) {
	return pty.Getsize(p.Ptmx)
}

// Write sends bytes to the child's stdin via the PTY master.
func (p *Process) Write(b []byte) (int, error) { return p.Ptmx.Write(b) }

// Pump copies PTY output to fn until EOF or error. It returns when the
// child's output stream closes (child exit or master closed).
func (p *Process) Pump(fn func([]byte)) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.Ptmx.Read(buf)
		if n > 0 {
			fn(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			// Linux PTY masters report EIO once the slave side is gone.
			if errors.Is(err, syscall.EIO) {
				return nil
			}
			return err
		}
	}
}

// Wait blocks until the child exits.
func (p *Process) Wait() error {
	<-p.waitCh
	return p.waitErr
}

// Close terminates the child and releases the PTY.
func (p *Process) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	err := p.Ptmx.Close()
	// Kill is unconditional: racing cmd.Wait() writes ProcessState, so we
	// cannot check it without a lock. Killing an exited process is harmless.
	_ = p.Cmd.Process.Kill()
	<-p.waitCh
	return err
}
