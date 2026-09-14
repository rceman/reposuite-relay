package term

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/fixtures"
)

func countFds(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(fds)
}

// TestTerminalChurn creates and disposes terminals repeatedly, checking for
// goroutine and fd leaks. (Gate: lifecycle sanity)
func TestTerminalChurn(t *testing.T) {
	const iters = 200
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseG := runtime.NumGoroutine()
	baseFDs := countFds(t)

	for i := 0; i < iters; i++ {
		m, _ := newTestModel(t, 80, 24)
		m.Write([]byte(fixtures.StyleTour))
		m.Write([]byte(fixtures.CodexProbes))
		_ = m.Snapshot(0)
		m.Term.Resize(100, 40)
		m.Dispose()
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	deltaG := runtime.NumGoroutine() - baseG
	deltaFD := countFds(t) - baseFDs
	t.Logf("goroutines: base=%d after=%d delta=%d; fds: base=%d after=%d delta=%d",
		baseG, runtime.NumGoroutine(), deltaG, baseFDs, countFds(t), deltaFD)
	if deltaG > 4 {
		t.Fatalf("goroutine leak: +%d after %d create/dispose cycles", deltaG, iters)
	}
	if deltaFD > 4 {
		t.Fatalf("fd leak: +%d after %d create/dispose cycles", deltaFD, iters)
	}
}
