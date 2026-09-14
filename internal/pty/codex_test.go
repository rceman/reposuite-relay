package pty_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/pty"
)

const codexFixturePath = "../../testdata/codex-startup.raw"

// rawCollector is shared with pty_test.go.

// TestCaptureCodexFixture spawns the real `codex` binary in an isolated HOME
// under our PTY and captures its raw startup stream. It NEVER touches the
// user's real ~/.codex, sessions, or running processes. Guarded: requires
// CAPTURE_CODEX=1 and a codex binary on PATH.
func TestCaptureCodexFixture(t *testing.T) {
	if os.Getenv("CAPTURE_CODEX") != "1" {
		t.Skip("set CAPTURE_CODEX=1 to regenerate the real codex fixture")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HOME="+home,
		"TERM=xterm-256color",
		"CODEX_HOME="+codexHome,
	)
	p, err := pty.Start(bin, nil, 140, 40, env, home)
	if err != nil {
		t.Fatalf("codex pty start: %v", err)
	}
	defer p.Close()

	raw := &rawCollector{}
	done := make(chan error, 1)
	go func() { done <- p.Pump(func(b []byte) { raw.add(b) }) }()

	time.Sleep(6 * time.Second)
	_ = p.Close()
	<-done

	data := []byte(raw.String())
	if len(data) == 0 {
		t.Fatal("codex produced no output")
	}
	if err := os.WriteFile(codexFixturePath, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Logf("captured %d bytes -> %s", len(data), codexFixturePath)
}
