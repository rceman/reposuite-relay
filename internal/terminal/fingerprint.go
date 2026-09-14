package terminal

import (
	"encoding/json"
	"fmt"
	"strings"

	xterm "github.com/rceman/xterm-go"
)

// Fingerprint is a canonical, comparable dump of a terminal's observable
// state: active-buffer viewport cells, cursor, modes, and scroll region.
// Two terminals with identical fingerprints are equivalent for Relay's
// purposes; plain-text similarity is NOT sufficient.
type Fingerprint struct {
	Cols          int        `json:"cols"`
	Rows          int        `json:"rows"`
	AltActive     bool       `json:"altActive"`
	CursorX       int        `json:"cursorX"`
	CursorY       int        `json:"cursorY"`
	CursorHidden  bool       `json:"cursorHidden"`
	ScrollTop     int        `json:"scrollTop"`
	ScrollBottom  int        `json:"scrollBottom"`
	YBase         int        `json:"ybase"`
	LinesTotal    int        `json:"linesTotal"`
	Modes         string     `json:"modes"`
	CurAttr       string     `json:"curAttr"`
	Rows_         [][]string `json:"cells"` // [viewport row][col] cell encoding
	NormalScrollN int        `json:"normalScrollback"`
}

// cellString encodes one buffer cell:
// "chars|width|fg|bg|flags" where fg/bg are "def", "pal:N" or "rgb:RRGGBB"
// and flags is a sorted letter set (b dim d italic u underline v inverse
// k blink n invisible s strike o overline).
func cellString(c *xterm.CellData) string {
	chars := c.GetChars()
	if chars == "" {
		chars = "·" // distinguish empty cells from absent chars
	}
	var flags strings.Builder
	if c.IsBold() != 0 {
		flags.WriteByte('b')
	}
	if c.IsDim() != 0 {
		flags.WriteByte('d')
	}
	if c.IsItalic() != 0 {
		flags.WriteByte('i')
	}
	if c.IsUnderline() != 0 {
		flags.WriteByte('u')
	}
	if c.IsInverse() != 0 {
		flags.WriteByte('v')
	}
	if c.IsBlink() != 0 {
		flags.WriteByte('k')
	}
	if c.IsInvisible() != 0 {
		flags.WriteByte('n')
	}
	if c.IsStrikethrough() != 0 {
		flags.WriteByte('s')
	}
	if c.IsOverline() != 0 {
		flags.WriteByte('o')
	}
	return fmt.Sprintf("%s|%d|%s|%s|%s", chars, c.GetWidth(), fgStr(&c.AttributeData), bgStr(&c.AttributeData), flags.String())
}

func fgStr(a *xterm.AttributeData) string {
	switch {
	case a.IsFgRGB():
		return fmt.Sprintf("rgb:%06x", a.GetFgColor())
	case a.IsFgPalette():
		return fmt.Sprintf("pal:%d", a.GetFgColor())
	default:
		return "def"
	}
}

func bgStr(a *xterm.AttributeData) string {
	switch {
	case a.IsBgRGB():
		return fmt.Sprintf("rgb:%06x", a.GetBgColor())
	case a.IsBgPalette():
		return fmt.Sprintf("pal:%d", a.GetBgColor())
	default:
		return "def"
	}
}

func modesString(t *xterm.Terminal) string {
	m := t.Modes()
	d := t.DecPrivateModes()
	mt := d.MouseTrackingMode
	if mt == "" {
		mt = "NONE"
	}
	me := d.MouseEncoding
	if me == "" {
		me = "DEFAULT"
	}
	return fmt.Sprintf(
		"ins=%v appcur=%v appkey=%v brpaste=%v origin=%v rewrap=%v sendfocus=%v wrap=%v mouse=%s mousenc=%s sync=%v",
		m.InsertMode, d.ApplicationCursorKeys, d.ApplicationKeypad, d.BracketedPasteMode,
		d.Origin, d.ReverseWraparound, d.SendFocus, d.Wraparound,
		mt, me, d.SynchronizedOutput,
	)
}

// Fingerprint dumps the terminal's observable state.
func TakeFingerprint(t *xterm.Terminal) *Fingerprint {
	buf := t.Buffer()
	rows := t.Rows()
	cols := t.Cols()
	fp := &Fingerprint{
		Cols:         cols,
		Rows:         rows,
		AltActive:    t.IsAltBufferActive(),
		CursorX:      t.CursorX(),
		CursorY:      t.CursorY(),
		CursorHidden: t.IsCursorHidden(),
		ScrollTop:    t.ScrollTop(),
		ScrollBottom: t.ScrollBottom(),
		YBase:        buf.YBase,
		LinesTotal:   buf.Lines.Length(),
		Modes:        modesString(t),
		Rows_:        make([][]string, rows),
	}
	ca := t.CurAttrData()
	fp.CurAttr = fmt.Sprintf("%s|%s|%s", fgStr(&ca), bgStr(&ca), curFlags(&ca))
	fp.NormalScrollN = t.NormalBuffer().Lines.Length() - t.Rows()

	cell := xterm.NewCellData()
	for y := 0; y < rows; y++ {
		line := buf.Lines.Get(buf.YBase + y)
		row := make([]string, cols)
		for x := 0; x < cols; x++ {
			if line != nil && x < line.Len {
				line.LoadCell(x, cell)
			} else {
				*cell = *xterm.NewCellData()
			}
			row[x] = cellString(cell)
		}
		fp.Rows_[y] = row
	}
	return fp
}

func curFlags(a *xterm.AttributeData) string {
	c := &xterm.CellData{AttributeData: *a}
	var flags strings.Builder
	for _, f := range []struct {
		ch  byte
		set uint32
	}{
		{'b', c.IsBold()}, {'d', c.IsDim()}, {'i', c.IsItalic()}, {'u', c.IsUnderline()},
		{'v', c.IsInverse()}, {'k', c.IsBlink()}, {'n', c.IsInvisible()},
		{'s', c.IsStrikethrough()}, {'o', c.IsOverline()},
	} {
		if f.set != 0 {
			flags.WriteByte(f.ch)
		}
	}
	return flags.String()
}

// JSON renders the fingerprint for cross-implementation comparison with the
// Node/xterm.js oracle.
func (f *Fingerprint) JSON() string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Diff returns a human-readable list of state differences; empty means equal.
func (f *Fingerprint) Diff(o *Fingerprint) []string {
	var out []string
	cmp := func(name string, a, b interface{}) {
		if a != b {
			out = append(out, fmt.Sprintf("%s: %v != %v", name, a, b))
		}
	}
	cmp("cols", f.Cols, o.Cols)
	cmp("rows", f.Rows, o.Rows)
	cmp("altActive", f.AltActive, o.AltActive)
	cmp("cursor", fmt.Sprintf("%d,%d", f.CursorX, f.CursorY), fmt.Sprintf("%d,%d", o.CursorX, o.CursorY))
	cmp("cursorHidden", f.CursorHidden, o.CursorHidden)
	cmp("scrollRegion", fmt.Sprintf("%d-%d", f.ScrollTop, f.ScrollBottom), fmt.Sprintf("%d-%d", o.ScrollTop, o.ScrollBottom))
	cmp("modes", f.Modes, o.Modes)
	cmp("curAttr", f.CurAttr, o.CurAttr)
	nr := len(f.Rows_)
	if len(o.Rows_) < nr {
		nr = len(o.Rows_)
	}
	for y := 0; y < nr; y++ {
		nc := len(f.Rows_[y])
		if len(o.Rows_[y]) < nc {
			nc = len(o.Rows_[y])
		}
		for x := 0; x < nc; x++ {
			if f.Rows_[y][x] != o.Rows_[y][x] {
				out = append(out, fmt.Sprintf("cell[%d,%d]: %q != %q", y, x, f.Rows_[y][x], o.Rows_[y][x]))
			}
		}
	}
	return out
}
