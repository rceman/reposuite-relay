package terminal

import (
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
	xterm "github.com/rceman/xterm-go"
)

// liveReset mirrors Airelay's LIVE_PRESENTATION_RESET prefix used before a
// replayed snapshot so a dirty destination is cleared first.
const liveReset = "\x1b[0m\x1b[H\x1b[2J\x1b[3J\x1b[H"

func freshTerm(t *testing.T, cols, rows int) *xterm.Terminal {
	t.Helper()
	term := xterm.New(xterm.WithCols(cols), xterm.WithRows(rows), xterm.WithScrollback(100))
	t.Cleanup(term.Dispose)
	return term
}

func snapshot(t *testing.T, term *xterm.Terminal, scrollback int) []byte {
	t.Helper()
	addon := xterm.NewSerializeAddon(term)
	var sb *int
	if scrollback >= 0 {
		sb = &scrollback
	}
	return addon.Serialize(&xterm.SerializeOptions{Scrollback: sb})
}

func assertSameState(t *testing.T, want, got *xterm.Terminal, ctx string) {
	t.Helper()
	if d := TakeFingerprint(want).Diff(TakeFingerprint(got)); len(d) > 0 {
		t.Fatalf("%s: state diverged (%d diffs):\n%s", ctx, len(d), strings.Join(d[:min(len(d), 8)], "\n"))
	}
}

// TestSnapshotRoundTrip: feed A -> serialize -> fresh B -> feed snapshot ->
// compare cells, cursor, modes, buffer. (Gate D)
func TestSnapshotRoundTrip(t *testing.T) {
	source := freshTerm(t, 80, 6)
	source.WriteString(fixtures.StyleTour)
	source.WriteString("\x1b[?2004h\x1b[?1h") // bracketed paste + app cursor keys

	dest := freshTerm(t, 80, 6)
	snap := snapshot(t, source, 0)
	dest.Write(snap)

	assertSameState(t, source, dest, "roundtrip")
	if !dest.DecPrivateModes().BracketedPasteMode || !dest.DecPrivateModes().ApplicationCursorKeys {
		t.Fatal("modes not restored")
	}
}

// TestSnapshotScrollback restores full scrollback content when requested.
func TestSnapshotScrollback(t *testing.T) {
	source := freshTerm(t, 40, 5)
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("LINE-")
		b.WriteString(strings.Repeat("x", 3))
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString("\r\n")
	}
	b.WriteString("TAIL")
	source.WriteString(b.String())

	dest := freshTerm(t, 40, 5)
	dest.Write(snapshot(t, source, -1)) // all scrollback

	if diff := TakeFingerprint(source).Diff(TakeFingerprint(dest)); len(diff) > 0 {
		t.Fatalf("scrollback snapshot diverged: %v", diff[:min(5, len(diff))])
	}
	if dest.NormalBuffer().Lines.Length() != source.NormalBuffer().Lines.Length() {
		t.Fatalf("scrollback length %d != %d",
			dest.NormalBuffer().Lines.Length(), source.NormalBuffer().Lines.Length())
	}
}

// TestSnapshotViewportOnly confirms scrollback:0 excludes history (Airelay's
// serializeLivePresentation semantics).
func TestSnapshotViewportOnly(t *testing.T) {
	source := freshTerm(t, 40, 5)
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("OLD_HISTORY_ROW\r\n")
	}
	b.WriteString("\x1b[2J\x1b[H\x1b[38;5;10mCURRENT_VIEW\x1b[0m")
	source.WriteString(b.String())

	snap := snapshot(t, source, 0)
	if strings.Contains(string(snap), "OLD_HISTORY_ROW") {
		t.Fatal("viewport snapshot leaked scrollback")
	}
	dest := freshTerm(t, 40, 5)
	dest.Write(snap)
	assertSameState(t, source, dest, "viewport-only")
}

// TestSnapshotSGRContinuation is THE regression gate (Gate E): an SGR left
// active at snapshot time must style subsequent output identically on both
// sides — without any new SGR being emitted.
func TestSnapshotSGRContinuation(t *testing.T) {
	source := freshTerm(t, 30, 4)
	source.WriteString("\x1b[48;5;236m\x1b[38;2;1;2;3m\x1b[1m\x1b[2;5H")

	dest := freshTerm(t, 30, 4)
	dest.Write(snapshot(t, source, 0))

	// Identical post-snapshot bytes, no SGR emitted.
	for _, term := range []*xterm.Terminal{source, dest} {
		term.WriteString("X")
	}

	assertSameState(t, source, dest, "sgr-continuation")

	c := xterm.NewCellData()
	line := dest.Buffer().Lines.Get(dest.Buffer().YBase + 1)
	line.LoadCell(4, c)
	if !c.IsBgPalette() || c.GetBgColor() != 236 {
		t.Fatalf("continuation bg=%v:%d want pal:236", c.IsBgPalette(), c.GetBgColor())
	}
	if !c.IsFgRGB() || c.GetFgColor() != 0x010203 {
		t.Fatalf("continuation fg=%v:%x want rgb:010203", c.IsFgRGB(), c.GetFgColor())
	}
	if c.IsBold() == 0 {
		t.Fatal("continuation lost bold")
	}
}

// TestSnapshotSGRContinuationMultiAttr pushes harder: italic+underline+inverse
// active at snapshot, continued after.
func TestSnapshotSGRContinuationMultiAttr(t *testing.T) {
	source := freshTerm(t, 30, 4)
	source.WriteString("\x1b[3;4;7m\x1b[38;5;99m\x1b[48;2;9;8;7m\x1b[3;3H")

	dest := freshTerm(t, 30, 4)
	dest.Write(snapshot(t, source, 0))
	for _, term := range []*xterm.Terminal{source, dest} {
		term.WriteString("abc")
	}
	assertSameState(t, source, dest, "multi-attr-continuation")

	c := xterm.NewCellData()
	dest.Buffer().Lines.Get(dest.Buffer().YBase+2).LoadCell(2, c)
	if c.IsItalic() == 0 || c.IsUnderline() == 0 || c.IsInverse() == 0 {
		t.Fatalf("lost flags italic=%d u=%d inv=%d", c.IsItalic(), c.IsUnderline(), c.IsInverse())
	}
	if !c.IsFgPalette() || c.GetFgColor() != 99 || !c.IsBgRGB() || c.GetBgColor() != 0x090807 {
		t.Fatalf("lost colors fg=%d bg=%x", c.GetFgColor(), c.GetBgColor())
	}
}

// TestSnapshotFutureOutputOrder prototypes the attach flow: snapshot at
// logical boundary N, then post-boundary bytes; a viewer restoring
// [snapshot, post-bytes] lands exactly at the authoritative state. (Gate E/D)
func TestSnapshotFutureOutputOrder(t *testing.T) {
	source := freshTerm(t, 60, 8)

	pre := "\x1b[1;1H\x1b[48;5;236mHEADER" + strings.Repeat(" ", 54) +
		"\x1b[3;1H\x1b[38;2;80;180;255mbody line one\x1b[0m\x1b[4;1H"
	post := "\x1b[38;5;11mpost-boundary styled\x1b[0m tail\x1b[7;7H\x1b[35mmag\x1b[0m"

	source.WriteString(pre)
	snap := snapshot(t, source, 0)
	source.WriteString(post)

	viewer := freshTerm(t, 60, 8)
	viewer.Write(snap)
	viewer.Write([]byte(post))

	assertSameState(t, source, viewer, "future-output-order")
}

// TestSnapshotAltBufferActive: snapshot while alternate buffer is active
// must restore both buffers and active-buffer selection. (Gate D, alt)
func TestSnapshotAltBufferActive(t *testing.T) {
	source := freshTerm(t, 30, 4)
	source.WriteString(fixtures.AltBufferUI)

	dest := freshTerm(t, 30, 4)
	dest.Write(snapshot(t, source, 0))

	if !dest.IsAltBufferActive() {
		t.Fatal("destination did not restore active alt buffer")
	}
	assertSameState(t, source, dest, "alt-buffer-active")

	// Leaving alt on dest reveals the same normal buffer as source.
	for _, term := range []*xterm.Terminal{source, dest} {
		term.WriteString("\x1b[?1049l")
	}
	assertSameState(t, source, dest, "alt-buffer-exit")
}

// TestSnapshotIntoDirtyDestination: restore over stale content requires the
// live-reset prefix (as Airelay does).
func TestSnapshotIntoDirtyDestination(t *testing.T) {
	source := freshTerm(t, 20, 4)
	source.WriteString("\x1b[3;1Hfinal\x1b[1;1H\x1b[48;5;25mhead")

	dest := freshTerm(t, 20, 4)
	dest.WriteString("STALE-1\r\nSTALE-2\r\nSTALE-3\r\nSTALE-4")

	snap := append([]byte(liveReset), snapshot(t, source, 0)...)
	dest.Write(snap)
	assertSameState(t, source, dest, "dirty-dest")
}

// TestSnapshotModes restores every mode the serializer covers.
func TestSnapshotModes(t *testing.T) {
	source := freshTerm(t, 80, 24)
	source.WriteString("\x1b[?1h\x1b[?66h\x1b[?2004h\x1b[4h\x1b[?6h\x1b[?45h\x1b[?1004h\x1b[?1002h\x1b[?7l\x1b[?25l")

	dest := freshTerm(t, 80, 24)
	dest.Write(snapshot(t, source, 0))
	assertSameState(t, source, dest, "modes")
}

// TestSnapshotScrollRegion restores a custom scroll region. Note: the
// serializer emits DECSTBM after the cursor-restore sequence, and DECSTBM
// homes the cursor — so a non-default scroll region costs the exact cursor
// position. This is upstream xterm.js addon behavior, not a Go divergence.
func TestSnapshotScrollRegion(t *testing.T) {
	source := freshTerm(t, 80, 24)
	source.WriteString("\x1b[5;20r\x1b[5;5Hinside")
	dest := freshTerm(t, 80, 24)
	dest.Write(snapshot(t, source, 0))
	if dest.ScrollTop() != source.ScrollTop() || dest.ScrollBottom() != source.ScrollBottom() {
		t.Fatalf("scroll region %d-%d != %d-%d",
			dest.ScrollTop(), dest.ScrollBottom(), source.ScrollTop(), source.ScrollBottom())
	}
	// Cells identical; cursor divergence is the documented DECSTBM quirk.
	d := TakeFingerprint(source).Diff(TakeFingerprint(dest))
	for _, line := range d {
		if !strings.HasPrefix(line, "cursor:") {
			t.Fatalf("unexpected diff: %s", line)
		}
	}
}

// TestSnapshotUnicodeCells round-trips wide/combining chars.
func TestSnapshotUnicodeCells(t *testing.T) {
	source := freshTerm(t, 20, 3)
	source.WriteString(fixtures.Unicode)
	dest := freshTerm(t, 20, 3)
	dest.Write(snapshot(t, source, 0))
	assertSameState(t, source, dest, "unicode-cells")
}

// TestSnapshotFullWidthBackground reproduces the Codex-style trailing
// background cells through serialize.
func TestSnapshotFullWidthBackground(t *testing.T) {
	source := freshTerm(t, 128, 5)
	source.WriteString(fixtures.FullWidthBackground(128))
	dest := freshTerm(t, 128, 5)
	dest.WriteString("stale-destination\r\nmore stale\r\n")
	dest.Write(append([]byte(liveReset), snapshot(t, source, 0)...))
	assertSameState(t, source, dest, "fullwidth-bg")
}
