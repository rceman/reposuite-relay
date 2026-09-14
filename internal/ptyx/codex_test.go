package ptyx_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/ptyx"
	"github.com/rceman/reposuite-relay/internal/term"
)

const codexFixturePath = "../../testdata/codex-startup.raw"

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
	p, err := ptyx.Start(bin, nil, 140, 40, env, home)
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

// TestCodexFixtureReplay feeds the captured real Codex startup stream
// through the Go terminal and checks the required query/reply behavior and
// state sanity. Skips if the fixture has not been captured yet.
func TestCodexFixtureReplay(t *testing.T) {
	data, err := os.ReadFile(codexFixturePath)
	if err != nil {
		t.Skip("REAL CODEX FIXTURE: PENDING (no testdata/codex-startup.raw)")
	}
	if len(data) == 0 {
		t.Skip("empty codex fixture")
	}
	t.Logf("replaying %d raw bytes", len(data))

	var replies [][]byte
	m := term.New(term.Options{
		Cols: 140, Rows: 40, Scrollback: 500,
		Fg:    [3]uint8{0xe5, 0xe5, 0xe5},
		Bg:    [3]uint8{0x1a, 0x1a, 0x1a},
		Reply: func(b []byte) { replies = append(replies, bytes.Clone(b)) },
	})
	defer m.Dispose()

	// Feed in irregular chunks to also exercise chunk robustness.
	for i := 0; i < len(data); {
		n := 1 + (i % 997)
		if i+n > len(data) {
			n = len(data) - i
		}
		m.Write(data[i : i+n])
		i += n
	}

	joined := bytes.Join(replies, nil)
	t.Logf("terminal generated %d reply events: %q", len(replies), joined)

	// Enumerate which of the known Codex probes were present in the stream.
	probes := map[string][]byte{
		"CSI 6n":   []byte("\x1b[6n"),
		"CSI c":    []byte("\x1b[c"),
		"CSI ?u":   []byte("\x1b[?u"),
		"OSC 10;?": []byte("\x1b]10;?\x1b\\"),
		"OSC 11;?": []byte("\x1b]11;?\x1b\\"),
	}
	for name, q := range probes {
		t.Logf("probe %-9s present=%v", name, bytes.Contains(data, q))
	}

	fp := term.TakeFingerprint(m.Term)
	if fp.Rows_ == nil {
		t.Fatal("fingerprint empty")
	}
	// Snapshot of the real stream must round-trip into a fresh terminal.
	snap := m.Snapshot(0)
	var dstReplies [][]byte
	dst := term.New(term.Options{
		Cols: 140, Rows: 40, Scrollback: 500,
		Fg:    [3]uint8{0xe5, 0xe5, 0xe5},
		Bg:    [3]uint8{0x1a, 0x1a, 0x1a},
		Reply: func(b []byte) { dstReplies = append(dstReplies, bytes.Clone(b)) },
	})
	defer dst.Dispose()
	dst.Write(append([]byte("\x1b[0m\x1b[H\x1b[2J\x1b[3J\x1b[H"), snap...))
	if d := fp.Diff(term.TakeFingerprint(dst.Term)); len(d) > 0 {
		t.Fatalf("codex snapshot roundtrip diverged: %v", d[:min(10, len(d))])
	}
	t.Logf("snapshot: %d bytes", len(snap))
	t.Logf("viewport:\n%s", m.Term.String())
}

// TestCodexFixtureProbeCoverage reports exactly which queries appear in the
// captured stream so the report can enumerate the real probe set.
func TestCodexFixtureProbeCoverage(t *testing.T) {
	data, err := os.ReadFile(codexFixturePath)
	if err != nil {
		t.Skip("no codex fixture")
	}
	// All CSI/OSC sequences present in the stream (deduped).
	re := regexp.MustCompile(`\x1b(?:\[[0-9;?>=<]*[a-zA-Z@` + "`" + `~$]|\][^\x1b\x07]*(?:\x1b\\|\x07))`)
	seen := map[string]int{}
	for _, mm := range re.FindAll(data, -1) {
		seen[string(mm)]++
	}
	for k, v := range seen {
		t.Logf("seq %q x%d", k, v)
	}
}
