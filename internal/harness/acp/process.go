package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rceman/reposuite-relay/internal/runtime"
)

// Command is the verified way to start one ACP agent: the vendor CLI itself
// with its `acp` subcommand on stdio. Relay never accepts a command from a
// client.
type Command struct {
	Path string
	Args []string
	Env  []string
}

// Resolve resolves an installed CLI by name — the only production command
// resolution Relay performs for a harness.
func Resolve(bin string, args ...string) (Command, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return Command{}, fmt.Errorf("%s executable: %w", bin, err)
	}
	return Command{
		Path: path,
		Args: args,
	}, nil
}

// Server is one owned ACP agent process generation plus its protocol client.
// The process is owned by the RuntimeSupervisor; this type owns only the
// transport and the typed protocol calls.
type Server struct {
	proc   *runtime.Process
	conn   *Conn
	client *Client
	stderr *tailBuffer

	mu      sync.Mutex
	serving bool
	exitErr error

	// initialized records that the handshake completed (test evidence).
	initialized atomic.Bool
}

// StartServer spawns the agent privately under Relay ownership and wires the
// ACP transport. The process is a direct exec in its own process group with
// stdin/stdout pipes and a bounded stderr tail; it is never exposed to
// clients, and there is no network listener anywhere in this path.
func StartServer(cmd Command, cwd string) (*Server, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	tail := newTailBuffer(StderrTailBytes)
	proc, err := runtime.Spawn(runtime.Spec{
		Path:      cmd.Path,
		Args:      cmd.Args,
		Dir:       cwd,
		Env:       cmd.Env,
		StdinPipe: true,
		Stdout:    pw,
		Stderr:    tail,
	})
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	// The child holds its own copy of the write end; closing ours means the
	// reader sees EOF exactly when the process dies.
	pw.Close()
	conn := NewConn(proc.Stdin(), pr)
	return &Server{
		proc:   proc,
		conn:   conn,
		client: NewClient(conn),
		stderr: tail,
	}, nil
}

// PID is the agent process id.
func (s *Server) PID() int { return s.proc.PID() }

// Wait closes when the process is terminated and reaped.
func (s *Server) Wait() <-chan struct{} { return s.proc.Wait() }

// Stop terminates the whole agent process tree (bounded, confirmed) and ends
// RPC correlation.
func (s *Server) Stop() error {
	s.conn.Close(ErrConnClosed)
	return s.proc.Stop()
}

// Client returns the typed protocol client.
func (s *Server) Client() *Client { return s.client }

// Conn returns the RPC engine (handler wiring).
func (s *Server) Conn() *Conn { return s.conn }

// Stderr returns the retained stderr tail (bounded diagnostics).
func (s *Server) Stderr() string { return s.stderr.String() }

// StderrSuffix formats the retained stderr for an error message.
func (s *Server) StderrSuffix() string {
	if tail := s.Stderr(); tail != "" {
		return " (stderr: " + tail + ")"
	}
	return ""
}

// Serve runs the transport reader until the process exits. The terminal
// error is recorded; pending calls fail with it. Exactly one caller.
func (s *Server) Serve() error {
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		return fmt.Errorf("acp server already serving")
	}
	s.serving = true
	s.mu.Unlock()
	err := s.conn.Start()
	s.mu.Lock()
	s.exitErr = err
	s.mu.Unlock()
	return err
}

// ExitErr returns the transport's terminal error (nil while live).
func (s *Server) ExitErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// Initialize performs the handshake and retains the reported capabilities.
func (s *Server) Initialize(ctx context.Context, client ClientInfo) (InitializeResult, error) {
	res, err := s.client.Initialize(ctx, client)
	if err == nil {
		s.initialized.Store(true)
	}
	return res, err
}

// Initialized reports whether the handshake completed.
func (s *Server) Initialized() bool { return s.initialized.Load() }

// tailBuffer keeps only the last max bytes of a stream (bounded stderr
// diagnostics — never a growing log).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// InitTimeout bounds the ACP handshake; CallTimeout bounds one control RPC.
const (
	InitTimeout = 20 * time.Second
	CallTimeout = 30 * time.Second
)

// CopyStderrTo copies the retained stderr tail to w (diagnostics only).
func (s *Server) CopyStderrTo(w io.Writer) {
	_, _ = io.WriteString(w, s.stderr.String())
}
