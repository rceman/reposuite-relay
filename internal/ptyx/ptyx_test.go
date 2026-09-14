package ptyx_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/ptyx"
	"github.com/rceman/reposuite-relay/internal/term"
)

// buildProbe compiles the fixture child once per test run.
func buildProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pty-probe-child")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/pty-probe-child").CombinedOutput()
	if err != nil {
		t.Fatalf("build probe child: %v\n%s", err, out)
	}
	return bin
}

type rawCollector struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *rawCollector) add(b []byte) {
	r.mu.Lock()
	r.buf.Write(b)
	r.mu.Unlock()
}

func (r *rawCollector) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func waitFor(t *testing.T, r *rawCollector, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if bytes.Contains([]byte(r.String()), []byte(want)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in PTY output:\n%q", want, r.String())
}

// TestZeroViewerQueryReply is the central feasibility proof: a child process
// emits Codex's startup probes; the Go terminal answers through the PTY with
// no physical terminal anywhere in the loop. (Gates A, C)
func TestZeroViewerQueryReply(t *testing.T) {
	bin := buildProbe(t)

	p, err := ptyx.Start(bin, nil, 120, 30, nil, "")
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer p.Close()

	raw := &rawCollector{}
	m := term.New(term.Options{
		Cols: 120, Rows: 30, Scrollback: 200,
		Fg: [3]uint8{0xe5, 0xe5, 0xe5},
		Bg: [3]uint8{0x1a, 0x1a, 0x1a},
		// Terminal-generated replies go straight back to the child's stdin.
		Reply: func(b []byte) { _, _ = p.Write(b) },
	})
	defer m.Dispose()

	pumpDone := make(chan error, 1)
	go func() {
		pumpDone <- p.Pump(func(b []byte) {
			raw.add(b)
			_, _ = m.Write(b)
		})
	}()

	waitFor(t, raw, "REPLY_HEX2", 15*time.Second)

	out := raw.String()
	re := regexp.MustCompile(`REPLY_HEX ([0-9a-f]*)`)
	m1 := re.FindStringSubmatch(out)
	if m1 == nil {
		t.Fatalf("no REPLY_HEX line in output:\n%q", out)
	}
	replyBytes, _ := hex.DecodeString(m1[1])
	t.Logf("child received replies: %q", string(replyBytes))

	expects := []string{
		"\x1b[1;1R",                        // CSI 6n cursor position
		"\x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\", // OSC 10 fg
		"\x1b]11;rgb:1a1a/1a1a/1a1a\x1b\\", // OSC 11 bg
		"\x1b[?0u",                         // CSI ?u kitty flags
		"\x1b[?1;2c",                       // CSI c device attributes
	}
	for _, e := range expects {
		if !bytes.Contains(replyBytes, []byte(e)) {
			t.Errorf("reply %q missing from child input %q", e, replyBytes)
		}
	}

	re2 := regexp.MustCompile(`REPLY_HEX2 ([0-9a-f]*)`)
	m2 := re2.FindStringSubmatch(out)
	if m2 == nil {
		t.Fatalf("no REPLY_HEX2 line in output:\n%q", out)
	}
	reply2, _ := hex.DecodeString(m2[1])
	if !bytes.Contains(reply2, []byte("\x1b[?1u")) {
		t.Fatalf("kitty push reply wrong: %q", reply2)
	}

	// Gate A proof: resize the live PTY; child reports new winsize.
	if err := p.Resize(100, 50); err != nil {
		t.Fatalf("pty resize: %v", err)
	}
	m.Term.Resize(100, 50)
	if _, err := p.Write([]byte("g")); err != nil {
		t.Fatalf("go byte: %v", err)
	}
	waitFor(t, raw, "PROBE_DONE", 15*time.Second)
	if !regexp.MustCompile(`WINSIZE 50x100`).MatchString(raw.String()) {
		t.Fatalf("winsize not propagated; output:\n%q", raw.String())
	}

	_ = p.Close()
	<-pumpDone

	// The terminal model tracked the whole session headlessly.
	fp := term.TakeFingerprint(m.Term)
	if fp.Cols != 100 || fp.Rows != 50 {
		t.Fatalf("model size %dx%d", fp.Cols, fp.Rows)
	}
	t.Logf("final viewport:\n%s", m.Term.String())
}

// TestPtyWinsizeAtSpawn verifies StartWithSize lands the requested geometry.
func TestPtyWinsizeAtSpawn(t *testing.T) {
	p, err := ptyx.Start("stty", []string{"size"}, 133, 41, nil, "")
	if err != nil {
		t.Skipf("stty unavailable: %v", err)
	}
	defer p.Close()
	raw := &rawCollector{}
	done := make(chan error, 1)
	go func() { done <- p.Pump(func(b []byte) { raw.add(b) }) }()
	waitFor(t, raw, "41 133", 5*time.Second)
	<-done
}

// TestPtyChurn spawns/closes PTY processes in a loop watching fds.
func TestPtyChurn(t *testing.T) {
	countFds := func() int {
		d, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("no /proc/self/fd")
		}
		return len(d)
	}
	base := countFds()
	for i := 0; i < 50; i++ {
		p, err := ptyx.Start("true", nil, 80, 24, nil, "")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		_ = p.Pump(func([]byte) {})
		if err := p.Close(); err != nil {
			t.Fatalf("iter %d close: %v", i, err)
		}
	}
	if d := countFds() - base; d > 4 {
		t.Fatalf("fd leak: +%d after 50 cycles", d)
	}
}
