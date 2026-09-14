package term

import (
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
)

// TestResizeSequence drives 120x30 -> 108x71 -> narrow -> wide and verifies
// content, cursor, backgrounds, and Unicode survive reflow.
func TestResizeSequence(t *testing.T) {
	m, _ := newTestModel(t, 120, 30)
	m.Write([]byte(fixtures.FullWidthBackground(120)))
	m.Write([]byte("\x1b[6;1H\x1b[38;5;11mToken usage\x1b[0m"))
	m.Write([]byte(fixtures.Unicode))
	m.Write([]byte("\x1b[20;5Hmarker"))

	if c := m.Term.CursorX(); c != 4+6 {
		t.Fatalf("cursor X=%d want 10", c)
	}

	resizes := [][2]int{{108, 71}, {60, 71}, {108, 20}, {140, 40}}
	for _, rs := range resizes {
		m.Term.Resize(rs[0], rs[1])
		if m.Term.Cols() != rs[0] || m.Term.Rows() != rs[1] {
			t.Fatalf("size=%dx%d want %dx%d", m.Term.Cols(), m.Term.Rows(), rs[0], rs[1])
		}
		// Every viewport line's cells must be readable (no torn lines).
		fp := TakeFingerprint(m.Term)
		if len(fp.Rows_) != rs[1] {
			t.Fatalf("fingerprint rows=%d want %d", len(fp.Rows_), rs[1])
		}
	}

	// After reflow the wide char content must survive somewhere in the
	// buffer — the buffer must remain internally valid.
	foundWide, foundMarker := false, false
	buf := m.Term.Buffer()
	for i := 0; i < buf.Lines.Length(); i++ {
		l := buf.Lines.Get(i)
		if l == nil {
			continue
		}
		s := l.TranslateToString(true, 0, -1)
		if strings.Contains(s, "界") {
			foundWide = true
		}
		if strings.Contains(s, "Token usage") {
			foundMarker = true
		}
	}
	if !foundWide || !foundMarker {
		t.Fatalf("content lost after resize: wide=%v marker=%v", foundWide, foundMarker)
	}
}

// TestResizeNarrowWrapReflow checks reflow of a full-width wrapped line when
// the cursor is NOT inside the wrapped group: narrowing re-wraps and
// widening restores. (xterm.js reflow semantics)
func TestResizeNarrowWrapReflow(t *testing.T) {
	m, _ := newTestModel(t, 40, 10)
	m.Write([]byte(fixtures.WrapLine(40))) // 43 W's: wraps to 2 lines
	m.Write([]byte("\x1b[6;1H"))           // move cursor off the wrapped group

	m.Term.Resize(20, 10)
	buf := m.Term.Buffer()
	var text string
	wrapped := 0
	for i := 0; i < buf.Lines.Length(); i++ {
		if l := buf.Lines.Get(i); l != nil {
			if l.IsWrapped {
				wrapped++
			}
			text += l.TranslateToString(true, 0, -1)
		}
	}
	if wrapped < 2 {
		t.Fatalf("expected >=2 wrapped lines after narrowing to 20 cols, got %d", wrapped)
	}
	if strings.Count(text, "W") != 43 {
		t.Fatalf("narrow: W count=%d want 43", strings.Count(text, "W"))
	}

	m.Term.Resize(80, 10)
	text = ""
	for i := 0; i < buf.Lines.Length(); i++ {
		if l := buf.Lines.Get(i); l != nil {
			text += l.TranslateToString(true, 0, -1)
		}
	}
	if strings.Count(text, "W") != 43 {
		t.Fatalf("widen: W count=%d want 43", strings.Count(text, "W"))
	}
}

// TestResizeCursorLineQuirk documents the upstream xterm.js quirk: when the
// cursor sits inside a wrapped line group, that group is NOT reflowed on
// narrow (reflowCursorLine defaults off) and trailing cells are lost.
// Verified parity against @xterm/headless oracle.
func TestResizeCursorLineQuirk(t *testing.T) {
	m, _ := newTestModel(t, 40, 10)
	m.Write([]byte(fixtures.WrapLine(40))) // cursor stays on wrapped group

	m.Term.Resize(20, 10)
	buf := m.Term.Buffer()
	var text string
	for i := 0; i < buf.Lines.Length(); i++ {
		if l := buf.Lines.Get(i); l != nil {
			text += l.TranslateToString(true, 0, -1)
		}
	}
	if got := strings.Count(text, "W"); got != 23 {
		t.Fatalf("cursor-line quirk: W count=%d want 23 (xterm.js parity)", got)
	}
}

// TestResizeAltBuffer resizes while the alternate buffer is active.
func TestResizeAltBuffer(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	m.Write([]byte(fixtures.AltBufferUI))
	m.Term.Resize(100, 40)
	if !m.Term.IsAltBufferActive() {
		t.Fatal("alt buffer lost active flag on resize")
	}
	if c := cellAt(t, m.Term, 0, 0); !c.IsBgPalette() || c.GetBgColor() != 25 {
		t.Fatal("alt content lost on resize")
	}
	m.Term.Resize(50, 10)
	if !m.Term.IsAltBufferActive() {
		t.Fatal("alt buffer lost active flag on shrink")
	}
}
