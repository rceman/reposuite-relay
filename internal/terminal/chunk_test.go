package terminal

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
)

// streams exercises every parser class: CSI, OSC (with ST), SGR, DCS-ish
// content, wide chars, combining chars, buffer switches, and queries.
func chunkStreams() map[string][]byte {
	return map[string][]byte{
		"style-tour":   []byte(fixtures.StyleTour),
		"codex-probes": []byte(fixtures.CodexProbes),
		"unicode":      []byte(fixtures.Unicode),
		"alt-buffer":   []byte(fixtures.AltBufferUI),
		"fullwidth-bg": []byte(fixtures.FullWidthBackground(128)),
		"mixed": []byte(fixtures.StyleTour + fixtures.AltBufferUI +
			"\x1b[?1049l" + fixtures.BCE + fixtures.Unicode + fixtures.CodexProbes),
	}
}

// TestChunkBoundaries proves arbitrary PTY chunk splits do not change final
// terminal state: whole-write vs byte-at-a-time vs seeded random splits.
func TestChunkBoundaries(t *testing.T) {
	for name, stream := range chunkStreams() {
		t.Run(name, func(t *testing.T) {
			reference, _ := newTestModel(t, 128, 24)
			if _, err := reference.Write(stream); err != nil {
				t.Fatal(err)
			}
			want := TakeFingerprint(reference.Term)

			// Byte-at-a-time.
			single, _ := newTestModel(t, 128, 24)
			for i := 0; i < len(stream); i++ {
				single.Write(stream[i : i+1])
			}
			if d := want.Diff(TakeFingerprint(single.Term)); len(d) > 0 {
				t.Fatalf("byte-wise writes diverged:\n%v", d[:min(5, len(d))])
			}

			// Seeded random chunkings.
			for seed := int64(0); seed < 8; seed++ {
				r := rand.New(rand.NewSource(seed))
				m, _ := newTestModel(t, 128, 24)
				for i := 0; i < len(stream); {
					n := 1 + r.Intn(7)
					if i+n > len(stream) {
						n = len(stream) - i
					}
					m.Write(stream[i : i+n])
					i += n
				}
				if d := want.Diff(TakeFingerprint(m.Term)); len(d) > 0 {
					t.Fatalf("seed %d random chunks diverged:\n%v", seed, d[:min(5, len(d))])
				}
			}
		})
	}
}

// TestSplitEscapeSequences feeds each query split at every possible boundary.
// Note: for OSC strings, the ESC of the ST terminator legitimately completes
// the sequence — a reply at that split is correct, not premature.
func TestSplitEscapeSequences(t *testing.T) {
	queries := []string{
		"\x1b[6n", "\x1b[c", "\x1b[?u", "\x1b]10;?\x1b\\", "\x1b]11;?\x1b\\",
	}
	for _, q := range queries {
		// Earliest split at which the sequence could already be complete:
		// for OSC, the ESC that opens the ST terminator aborts the OSC.
		earlyComplete := len(q)
		if i := strings.Index(q[2:], "\x1b"); i >= 0 {
			earlyComplete = i + 3 // index of terminator ESC + 1
		}
		for split := 1; split < len(q); split++ {
			m, replies := newTestModel(t, 80, 24)
			m.Write([]byte(q[:split]))
			if split < earlyComplete && len(*replies) != 0 {
				t.Fatalf("query %q split %d: premature reply", q, split)
			}
			m.Write([]byte(q[split:]))
			if len(*replies) == 0 {
				t.Fatalf("query %q split %d: no reply after completion", q, split)
			}
		}
	}
}
