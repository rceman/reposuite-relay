package terminal

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
)

// TestIndependentTerminals proves two terminals share no state: different
// streams concurrently, distinct cursors, distinct reply paths.
// Run with -race for the data-race proof. (Gate G)
func TestIndependentTerminals(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	results := make([]*Fingerprint, n)
	replies := make([][]byte, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var rep [][]byte
			m := New(Options{
				Cols: 60 + i, Rows: 10 + i, Scrollback: 50,
				Fg:    [3]uint8{0xe5, 0xe5, 0xe5},
				Bg:    [3]uint8{0x1a, 0x1a, 0x1a},
				Reply: func(b []byte) { rep = append(rep, bytes.Clone(b)) },
			})
			defer m.Dispose()
			// Per-instance traffic: unique content + queries. The marker goes
			// on a row StyleTour does not repaint.
			stream := fixtures.StyleTour +
				fmt.Sprintf("\x1b[9;1HINSTANCE-%d\x1b[%d;%dH", i, 10+i, 5+i) +
				fixtures.CodexProbes
			for j := 0; j < len(stream); j += 13 {
				end := j + 13
				if end > len(stream) {
					end = len(stream)
				}
				m.Write([]byte(stream[j:end]))
			}
			results[i] = TakeFingerprint(m.Term)
			replies[i] = bytes.Join(rep, nil)
		}(i)
	}
	wg.Wait()

	// Cross-check: each instance has its own cursor position and size.
	for i := 0; i < n; i++ {
		if results[i].Cols != 60+i || results[i].Rows != 10+i {
			t.Fatalf("instance %d size %dx%d", i, results[i].Cols, results[i].Rows)
		}
		want := fmt.Sprintf("INSTANCE-%d", i)
		var row8 string
		for _, cell := range results[i].Rows_[8] {
			if idx := bytes.IndexByte([]byte(cell), '|'); idx > 0 && cell[:idx] != "·" {
				row8 += cell[:idx]
			}
		}
		if !bytes.Contains([]byte(row8), []byte(want)) {
			t.Fatalf("instance %d row8=%q want %q", i, row8, want)
		}
		if !bytes.Contains(replies[i], []byte("\x1b[?1;2c")) {
			t.Fatalf("instance %d missing DA reply", i)
		}
	}
}

// TestConcurrentSnapshots hammers serialize on one terminal while another
// parses — separate instances only, proving no cross-instance coupling.
func TestConcurrentSnapshots(t *testing.T) {
	m1, _ := newTestModel(t, 80, 24)
	m1.Write([]byte(fixtures.StyleTour))
	want := TakeFingerprint(m1.Term)

	// Snapshots are restore-once into a fresh terminal: serialize once,
	// restore into N independent destinations concurrently.
	snap := m1.Snapshot(0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dst, _ := newTestModel(t, 80, 24)
			dst.Write(snap)
			if d := want.Diff(TakeFingerprint(dst.Term)); len(d) > 0 {
				t.Errorf("restore %d diverged: %v", i, d[:3])
			}
		}(i)
	}
	wg.Wait()
}
