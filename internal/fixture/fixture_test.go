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
// Process stop semantics (bounded TERM/KILL, group reap) are covered by
// internal/runtime's process tests; this file only proves the fixture
// wrapper wiring.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("FIXTURE_HELPER") {
	case "":
		return
	case "eof": // normal fixture behavior: block on stdin, exit 0 on EOF
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

// TestFixtureChildExitsOnEOF: the fixture child is cooperative — closing
// its stdin pipe terminates it, and the stop is confirmed (reaped).
func TestFixtureChildExitsOnEOF(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "FIXTURE_HELPER=eof")
	cmd.Dir = t.TempDir()
	p, err := SpawnCmd(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID() <= 0 {
		t.Fatalf("bad pid %d", p.PID())
	}
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stop exceeded the bounded waits")
	}
	if !p.Exited() {
		t.Fatal("child not reaped after stop")
	}
}
