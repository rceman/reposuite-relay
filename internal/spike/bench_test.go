// Package spike hosts the performance probe: throughput of parsing a large
// ANSI stream, snapshot cost, resize cost, and memory footprint.
package spike

import (
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/term"
)

// genStream builds ~target bytes of realistic terminal traffic: styled text
// lines, indexed/RGB colors, cursor moves, scroll — Codex/TUI-shaped.
func genStream(target int, cols int) []byte {
	var b strings.Builder
	b.Grow(target + 1024)
	r := rand.New(rand.NewSource(42))
	styles := []string{
		"\x1b[0m", "\x1b[1m", "\x1b[3m", "\x1b[4m",
		"\x1b[38;5;196m", "\x1b[38;5;45m", "\x1b[48;5;236m",
		"\x1b[38;2;200;120;60m", "\x1b[48;2;10;20;30m",
	}
	words := []string{"codex", "relay", "terminal", "viewport", "snapshot", "resume", "buffer", "prompt"}
	line := 0
	for b.Len() < target {
		row := 1 + r.Intn(24)
		b.WriteString("\x1b[" + itoa(row) + ";1H")
		for b.Len() < target && r.Intn(100) < 92 {
			b.WriteString(styles[r.Intn(len(styles))])
			n := 3 + r.Intn(12)
			for i := 0; i < n; i++ {
				b.WriteString(words[r.Intn(len(words))])
				b.WriteByte(' ')
			}
			if r.Intn(10) < 2 {
				b.WriteString("\x1b[K")
			}
			line++
			if line%3 == 0 {
				b.WriteString("\r\n")
			}
		}
	}
	return []byte(b.String())
}

func itoa(i int) string { return strconv.Itoa(i) }

func rssMiB() float64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				return v / 1024
			}
		}
	}
	return -1
}

// TestPerf10MiB parses 10 MiB of ANSI traffic and reports timing/memory.
func TestPerf10MiB(t *testing.T) {
	const target = 10 << 20
	stream := genStream(target, 120)
	t.Logf("stream bytes: %d", len(stream))

	runtime.GC()
	baseRSS := rssMiB()

	m := term.New(term.Options{
		Cols: 120, Rows: 40, Scrollback: 10_000,
		Fg:    [3]uint8{0xe5, 0xe5, 0xe5},
		Bg:    [3]uint8{0x1a, 0x1a, 0x1a},
		Reply: func([]byte) {},
	})
	defer m.Dispose()

	start := time.Now()
	const chunk = 64 * 1024
	for i := 0; i < len(stream); i += chunk {
		end := i + chunk
		if end > len(stream) {
			end = len(stream)
		}
		m.Write(stream[i:end])
	}
	parseDur := time.Since(start)

	midRSS := rssMiB()

	start = time.Now()
	snap := m.Snapshot(0)
	serDur := time.Since(start)

	start = time.Now()
	m.Term.Resize(100, 50)
	resizeDur := time.Since(start)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	endRSS := rssMiB()

	t.Logf("parse+write: %v (%.1f MiB/s)", parseDur, float64(len(stream))/(1<<20)/parseDur.Seconds())
	t.Logf("snapshot serialize: %v (%d bytes)", serDur, len(snap))
	t.Logf("resize 120x40->100x50: %v", resizeDur)
	t.Logf("RSS: base=%.1f mid=%.1f end=%.1f MiB; heap inuse=%.1f MiB",
		baseRSS, midRSS, endRSS, float64(ms.HeapInuse)/(1<<20))

	if parseDur > 10*time.Second {
		t.Fatalf("parse too slow: %v", parseDur)
	}
}

// BenchmarkParseWrite is the formal benchmark for throughput.
func BenchmarkParseWrite(b *testing.B) {
	stream := genStream(4<<20, 120)
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := term.New(term.Options{Cols: 120, Rows: 40, Scrollback: 1000, Reply: func([]byte) {}})
		for j := 0; j < len(stream); j += 65536 {
			end := j + 65536
			if end > len(stream) {
				end = len(stream)
			}
			m.Write(stream[j:end])
		}
		m.Dispose()
	}
}

// BenchmarkSerialize measures snapshot cost on a populated terminal.
func BenchmarkSerialize(b *testing.B) {
	m := term.New(term.Options{Cols: 120, Rows: 40, Scrollback: 5000, Reply: func([]byte) {}})
	stream := genStream(2<<20, 120)
	m.Write(stream)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Snapshot(0)
	}
	b.StopTimer()
	m.Dispose()
}

// TestStartupTime measures terminal construction cost.
func TestStartupTime(t *testing.T) {
	start := time.Now()
	const n = 1000
	for i := 0; i < n; i++ {
		m := term.New(term.Options{Cols: 120, Rows: 40, Scrollback: 1000, Reply: func([]byte) {}})
		m.Dispose()
	}
	t.Logf("terminal create+dispose: %v each (%d iterations)", time.Since(start)/n, n)
}

// TestBinarySize builds relay-spike and reports the binary size.
func TestBinarySize(t *testing.T) {
	bin := t.TempDir() + "/relay-spike"
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/relay-spike").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("relay-spike binary: %.2f MiB", float64(fi.Size())/(1<<20))
}
