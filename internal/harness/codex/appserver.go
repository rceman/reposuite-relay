package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/rceman/reposuite-relay/internal/runtime"
)

// StderrTailBytes bounds the retained app-server stderr diagnostics.
const StderrTailBytes = 8 << 10

// Command is the verified way to start the app-server: the Codex CLI
// itself with the `app-server` subcommand on stdio.
type Command struct {
	Path string
	Args []string
}

// DefaultCommand resolves the installed Codex CLI. It is the only
// production command Relay ever runs for this harness.
func DefaultCommand() (Command, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return Command{}, fmt.Errorf("codex executable: %w", err)
	}
	return Command{
		Path: path,
		Args: []string{"app-server"},
	}, nil
}

// Server is one owned app-server process generation plus its RPC
// connection. The process is owned by the RuntimeSupervisor; this type
// owns only the transport and the typed protocol calls.
type Server struct {
	proc   *runtime.Process
	conn   *Conn
	stderr *TailBuffer

	mu      sync.Mutex
	info    InitializeResult
	started bool
	serving bool
	exitErr error
}

// StartServer spawns the app-server privately under Relay ownership and
// wires the JSON-RPC transport. The process is a direct exec in its own
// process group with stdin/stdout pipes and a bounded stderr tail; it is
// never exposed to clients.
func StartServer(cmd Command, cwd string) (*Server, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	tail := NewTailBuffer(StderrTailBytes)
	proc, err := runtime.Spawn(runtime.Spec{
		Path:      cmd.Path,
		Args:      cmd.Args,
		Dir:       cwd,
		StdinPipe: true,
		Stdout:    pw,
		Stderr:    tail,
	})
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	// The child holds its own copy of the write end; closing ours means
	// the reader sees EOF exactly when the process dies.
	pw.Close()
	return &Server{
		proc:   proc,
		conn:   NewConn(proc.Stdin(), pr),
		stderr: tail,
	}, nil
}

// PID is the app-server process id.
func (s *Server) PID() int { return s.proc.PID() }

// Wait closes when the process is terminated and reaped.
func (s *Server) Wait() <-chan struct{} { return s.proc.Wait() }

// Stop terminates the whole app-server process tree (bounded, confirmed)
// and ends RPC correlation.
func (s *Server) Stop() error {
	s.conn.Close(ErrConnClosed)
	return s.proc.Stop()
}

// Conn exposes the RPC engine (notification/request handler wiring).
func (s *Server) Conn() *Conn { return s.conn }

// Info returns the initialize handshake result.
func (s *Server) Info() InitializeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Stderr returns the retained stderr tail (bounded diagnostics).
func (s *Server) Stderr() string { return s.stderr.String() }

// Serve runs the transport reader until the process exits. The terminal
// error is recorded; pending calls fail with it. Exactly one caller.
func (s *Server) Serve() error {
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		return fmt.Errorf("app-server already serving")
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

// Initialize performs the verified handshake. It must be the first call.
func (s *Server) Initialize(ctx context.Context, client ClientInfo) (InitializeResult, error) {
	var res InitializeResult
	params := InitializeParams{ClientInfo: client}
	if err := s.conn.Call(ctx, "initialize", params, &res); err != nil {
		return res, err
	}
	s.mu.Lock()
	s.info = res
	s.started = true
	s.mu.Unlock()
	return res, nil
}

// ThreadStart creates a native thread. The returned thread ID is the
// exact native session identity.
func (s *Server) ThreadStart(ctx context.Context, p ThreadStartParams) (ThreadStartResult, error) {
	var res ThreadStartResult
	err := s.conn.Call(ctx, "thread/start", p, &res)
	return res, err
}

// ThreadResume reattaches to an exact native thread. There is no
// "newest"/"last" variant: a resume failure is reported, never papered
// over with a fresh thread.
func (s *Server) ThreadResume(ctx context.Context, p ThreadResumeParams) (ThreadResumeResult, error) {
	var res ThreadResumeResult
	err := s.conn.Call(ctx, "thread/resume", p, &res)
	return res, err
}

// TurnStart submits one turn to an exact thread.
func (s *Server) TurnStart(ctx context.Context, p TurnStartParams) (TurnStartResult, error) {
	var res TurnStartResult
	err := s.conn.Call(ctx, "turn/start", p, &res)
	return res, err
}

// TurnInterrupt cancels an in-flight turn by exact thread+turn ID.
func (s *Server) TurnInterrupt(ctx context.Context, p TurnInterruptParams) error {
	return s.conn.Call(ctx, "turn/interrupt", p, nil)
}

// CopyStderrTo copies the retained stderr tail to w (diagnostics only —
// bounded, never a growing log).
func (s *Server) CopyStderrTo(w io.Writer) {
	_, _ = io.WriteString(w, s.stderr.String())
}
