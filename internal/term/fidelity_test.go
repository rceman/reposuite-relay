package term

import (
	"strings"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
	"github.com/rceman/reposuite-relay/internal/fixtures"
)

// cellAt loads one cell from the active buffer's viewport row/col.
func cellAt(t *testing.T, term *xterm.Terminal, row, col int) *xterm.CellData {
	t.Helper()
	buf := term.Buffer()
	line := buf.Lines.Get(buf.YBase + row)
	if line == nil {
		t.Fatalf("no line at viewport row %d", row)
	}
	c := xterm.NewCellData()
	if line.LoadCell(col, c) == nil {
		t.Fatalf("no cell at %d,%d", row, col)
	}
	return c
}

// TestStyleCells checks per-cell attributes — not translated text — for the
// full SGR tour.
func TestStyleCells(t *testing.T) {
	m, _ := newTestModel(t, 120, 24)
	m.Write([]byte(fixtures.StyleTour))

	checks := []struct {
		name      string
		row, col  int
		wantChars string
		assert    func(t *testing.T, c *xterm.CellData)
	}{
		{"red fg", 0, 0, "r", func(t *testing.T, c *xterm.CellData) {
			if !c.IsFgPalette() || c.GetFgColor() != 1 {
				t.Fatalf("fg=%v:%d", c.IsFgPalette(), c.GetFgColor())
			}
		}},
		{"indexed fg 123", 0, 4, "i", func(t *testing.T, c *xterm.CellData) {
			if !c.IsFgPalette() || c.GetFgColor() != 123 {
				t.Fatalf("fg=%d", c.GetFgColor())
			}
		}},
		{"rgb fg 010203", 0, 12, "t", func(t *testing.T, c *xterm.CellData) {
			if !c.IsFgRGB() || c.GetFgColor() != 0x010203 {
				t.Fatalf("rgb fg=%x", c.GetFgColor())
			}
		}},
		{"indexed bg 25", 0, 22, "i", func(t *testing.T, c *xterm.CellData) {
			if !c.IsBgPalette() || c.GetBgColor() != 25 {
				t.Fatalf("bg=%d", c.GetBgColor())
			}
		}},
		{"rgb bg", 0, 33, "r", func(t *testing.T, c *xterm.CellData) {
			if !c.IsBgRGB() || c.GetBgColor() != 0x090807 {
				t.Fatalf("rgb bg=%x", c.GetBgColor())
			}
		}},
		{"bold", 0, 40, "b", func(t *testing.T, c *xterm.CellData) {
			if c.IsBold() == 0 {
				t.Fatal("not bold")
			}
		}},
		{"dim", 0, 45, "d", func(t *testing.T, c *xterm.CellData) {
			if c.IsDim() == 0 {
				t.Fatal("not dim")
			}
		}},
		{"italic", 0, 49, "i", func(t *testing.T, c *xterm.CellData) {
			if c.IsItalic() == 0 {
				t.Fatal("not italic")
			}
		}},
		{"underline", 0, 56, "u", func(t *testing.T, c *xterm.CellData) {
			if c.IsUnderline() == 0 {
				t.Fatal("not underline")
			}
		}},
		{"blink", 0, 66, "b", func(t *testing.T, c *xterm.CellData) {
			if c.IsBlink() == 0 {
				t.Fatal("not blink")
			}
		}},
		{"inverse", 0, 72, "i", func(t *testing.T, c *xterm.CellData) {
			if c.IsInverse() == 0 {
				t.Fatal("not inverse")
			}
		}},
		{"invisible", 0, 80, "i", func(t *testing.T, c *xterm.CellData) {
			if c.IsInvisible() == 0 {
				t.Fatal("not invisible")
			}
		}},
		{"strikethrough", 0, 90, "s", func(t *testing.T, c *xterm.CellData) {
			if c.IsStrikethrough() == 0 {
				t.Fatal("not strike")
			}
		}},
		{"overline", 0, 97, "o", func(t *testing.T, c *xterm.CellData) {
			if c.IsOverline() == 0 {
				t.Fatal("not overline")
			}
		}},
		{"bright fg 92", 1, 0, "b", func(t *testing.T, c *xterm.CellData) {
			if !c.IsFgPalette() || c.GetFgColor() != 10 {
				t.Fatalf("bright fg=%d", c.GetFgColor())
			}
		}},
		{"bright bg 103", 1, 10, "b", func(t *testing.T, c *xterm.CellData) {
			if !c.IsBgPalette() || c.GetBgColor() != 11 {
				t.Fatalf("bright bg=%d", c.GetBgColor())
			}
		}},
	}
	for _, chk := range checks {
		t.Run(chk.name, func(t *testing.T) {
			c := cellAt(t, m.Term, chk.row, chk.col)
			if c.GetChars() != chk.wantChars {
				t.Fatalf("chars=%q want %q", c.GetChars(), chk.wantChars)
			}
			chk.assert(t, c)
		})
	}
}

// TestFullWidthBackground verifies trailing blank cells carry the palette
// background — the Codex-style prompt rows.
func TestFullWidthBackground(t *testing.T) {
	const cols = 128
	m, _ := newTestModel(t, cols, 24)
	m.Write([]byte(fixtures.FullWidthBackground(cols)))

	for _, col := range []int{0, 10, cols - 1} {
		c := cellAt(t, m.Term, 0, col)
		if !c.IsBgPalette() || c.GetBgColor() != 236 {
			t.Fatalf("row0 col%d bg=%v:%d", col, c.IsBgPalette(), c.GetBgColor())
		}
		c = cellAt(t, m.Term, 2, col)
		if !c.IsBgPalette() || c.GetBgColor() != 236 {
			t.Fatalf("row2 col%d bg=%v:%d", col, c.IsBgPalette(), c.GetBgColor())
		}
	}
	if c := cellAt(t, m.Term, 1, 0); !c.IsBgDefault() {
		t.Fatal("untouched row must keep default bg")
	}
}

// TestBCE verifies erase-line fills cells with the current background (BCE).
func TestBCE(t *testing.T) {
	m, _ := newTestModel(t, 128, 24)
	m.Write([]byte(fixtures.BCE))
	c := cellAt(t, m.Term, 0, 127)
	if !c.IsBgPalette() || c.GetBgColor() != 236 {
		t.Fatalf("BCE cell bg=%v:%d want pal:236", c.IsBgPalette(), c.GetBgColor())
	}
	if c.GetChars() != "" {
		t.Fatalf("BCE cell chars=%q want empty", c.GetChars())
	}
	// "BCE row" text written after reset must be default bg.
	c = cellAt(t, m.Term, 0, 0)
	if !c.IsBgDefault() || c.GetChars() != "B" {
		t.Fatalf("text cell bg=%v chars=%q", c.IsBgDefault(), c.GetChars())
	}
}

// TestUnicode verifies wide chars occupy 2 cells (width 2 + width-0
// continuation) and combining sequences stay in one cell.
func TestUnicode(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	m.Write([]byte(fixtures.Unicode))

	if c := cellAt(t, m.Term, 0, 0); c.GetChars() != "界" || c.GetWidth() != 2 {
		t.Fatalf("wide char: chars=%q width=%d", c.GetChars(), c.GetWidth())
	}
	if c := cellAt(t, m.Term, 0, 1); c.GetWidth() != 0 {
		t.Fatalf("continuation cell width=%d", c.GetWidth())
	}
	if c := cellAt(t, m.Term, 0, 2); c.GetChars() != "e\u0301" {
		t.Fatalf("combined cell chars=%q", c.GetChars())
	}
	if c := cellAt(t, m.Term, 0, 3); c.GetChars() != "한" || c.GetWidth() != 2 {
		t.Fatalf("wide char2: chars=%q width=%d", c.GetChars(), c.GetWidth())
	}
	if c := cellAt(t, m.Term, 2, 0); c.GetChars() != "e\u0301" {
		t.Fatalf("row2 combined cell chars=%q", c.GetChars())
	}
	if c := cellAt(t, m.Term, 2, 2); c.GetChars() != "a\u0308" {
		t.Fatalf("row2 cell2 chars=%q", c.GetChars())
	}
}

// TestAlternateBuffer proves active-buffer tracking across 1049h/l.
func TestAlternateBuffer(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	m.Write([]byte(fixtures.AltBufferUI))

	if !m.Term.IsAltBufferActive() {
		t.Fatal("alt buffer not active after 1049h")
	}
	if c := cellAt(t, m.Term, 0, 0); c.GetChars() != " " || !c.IsBgPalette() || c.GetBgColor() != 25 {
		t.Fatalf("alt title cell chars=%q bg=%v:%d", c.GetChars(), c.IsBgPalette(), c.GetBgColor())
	}
	if c := cellAt(t, m.Term, 2, 1); c.GetChars() != "a" || !c.IsFgPalette() || c.GetFgColor() != 196 {
		t.Fatalf("alt body cell chars=%q fg=%v:%d", c.GetChars(), c.IsFgPalette(), c.GetFgColor())
	}
	// Normal buffer retained its history while alt was active.
	if got := m.Term.NormalBuffer().Lines.Get(0).TranslateToString(true, 0, -1); !strings.Contains(got, "normal-history-1") {
		t.Fatalf("normal buffer line0=%q", got)
	}

	m.Write([]byte("\x1b[?1049l"))
	if m.Term.IsAltBufferActive() {
		t.Fatal("alt buffer still active after 1049l")
	}
	if got := m.Term.GetLine(0); !strings.Contains(got, "normal-history-1") {
		t.Fatalf("normal viewport line0=%q", got)
	}
}
