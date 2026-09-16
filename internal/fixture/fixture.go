// Package fixture implements the deterministic fixture harness used to
// prove daemon/session ownership before real harnesses exist.
//
// The fixture child is the SAME executable in the hidden `__fixture` mode.
// It blocks reading a daemon-owned stdin pipe until EOF, so it always exits
// when the daemon stops it — and, crucially, when the daemon dies for any
// reason (the kernel closes the pipe's write end). Fixture processes can
// therefore never outlive their daemon.
//
// Process ownership lives in internal/runtime (the RuntimeSupervisor's
// process primitive): the fixture is a dedicated, one-process-per-session
// harness with its own process group and a bounded stop path. It executes
// no client-supplied command, needs no PTY, and needs no terminal model.
package fixture

import (
	"io"
	"os"
	"os/exec"

	"github.com/rceman/reposuite-relay/internal/runtime"
)

// Spawn starts `selfExe __fixture` in cwd with a daemon-owned stdin pipe.
// The child is a direct exec (never a shell) in its own process group.
func Spawn(selfExe, cwd string) (*runtime.Process, error) {
	return runtime.Spawn(runtime.Spec{
		Path:      selfExe,
		Args:      []string{"__fixture"},
		Dir:       cwd,
		StdinPipe: true,
	})
}

// SpawnCmd adopts an already-prepared *exec.Cmd as a fixture child — the
// seam tests use to prove forced-kill semantics with a controlled
// stubborn process. cmd's StdinPipe is taken over; Dir/Env are the
// caller's.
func SpawnCmd(cmd *exec.Cmd) (*runtime.Process, error) {
	return runtime.Adopt(cmd)
}

// RunChild is the `__fixture` child entry point: block on stdin until EOF,
// then exit 0. EOF arrives when the daemon closes the pipe (stop) or dies
// (kernel cleanup), so the fixture can never be orphaned.
func RunChild() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
