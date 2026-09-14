// Package fixtures holds deterministic terminal byte streams used across the
// spike tests. Several fixtures mirror Airelay's controller-presentation
// oracle tests (test/controller-presentation.test.ts @ eef8d94).
package fixtures

import "strings"

// CodexProbes is the exact startup probe batch Codex TUI emits on Unix
// (codex-rs/tui/src/terminal_probe.rs): CSI 6n, OSC 10;?, OSC 11;?, CSI ?u,
// CSI c, batched under one deadline.
const CodexProbes = "\x1b[6n\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b[?u\x1b[c"

// StyleTour exercises every SGR family Relay cares about: standard/bright/
// indexed/RGB colors, all attribute flags, and resets. Mirrors the Airelay
// round-trip fixture.
const StyleTour = "\x1b[1;1H" +
	"\x1b[31mred\x1b[0m " +
	"\x1b[38;5;123mindexed\x1b[0m " +
	"\x1b[38;2;1;2;3mtruecolor\x1b[0m " +
	"\x1b[48;5;25mindexed-bg\x1b[0m " +
	"\x1b[48;2;9;8;7mrgb-bg\x1b[0m " +
	"\x1b[1mbold\x1b[22m " +
	"\x1b[2mdim\x1b[22m " +
	"\x1b[3mitalic\x1b[23m " +
	"\x1b[4munderline\x1b[24m " +
	"\x1b[5mblink\x1b[25m " +
	"\x1b[7minverse\x1b[27m " +
	"\x1b[8minvisible\x1b[28m " +
	"\x1b[9mstrike\x1b[29m " +
	"\x1b[53moverline\x1b[55m" +
	"\x1b[2;1H\x1b[92mbright-fg\x1b[0m \x1b[103mbright-bg\x1b[0m" +
	"\x1b[6;12H"

// FullWidthBackground paints a Codex-like full-width palette background row
// (prompt line + input line with trailing blank cells) — the case Airelay
// regressed on before adopting the official serializer.
func FullWidthBackground(cols int) string {
	prompt := "› hey"
	input := "› Ask Codex to do anything"
	return "\x1b[1;1H\x1b[48;5;236m" +
		prompt + strings.Repeat(" ", cols-len([]rune(prompt))) +
		"\x1b[3;1H" +
		input + strings.Repeat(" ", cols-len([]rune(input))) +
		"\x1b[5;1H\x1b[38;5;11mstatus\x1b[0m"
}

// BCE exercises background-color erase: fill a line's background with EL 2
// after setting a palette bg.
const BCE = "\x1b[1;1H\x1b[48;5;236m\x1b[2K\x1b[1;1H\x1b[0mBCE row"

// Unicode mixes wide CJK chars and combining sequences.
const Unicode = "\x1b[1;1H\u754ce\u0301\ud55c\r\n" +
	"\x1b[2;1H\u4e2d\u6587\u5b57 tail\r\n" +
	"\x1b[3;1He\u0301 a\u0308 z"

// AltBufferUI enters the alternate buffer, draws a UI, and stays there.
const AltBufferUI = "normal-history-1\r\nnormal-history-2\r\n" +
	"\x1b[?1049h\x1b[H\x1b[2J" +
	"\x1b[1;1H\x1b[48;5;25m ALT-UI TITLE \x1b[0m" +
	"\x1b[3;2H\x1b[38;5;196malternate body\x1b[0m" +
	"\x1b[6;10H"

// WrapLine forces a hard wrap at the right edge (for chunk/resize tests).
func WrapLine(cols int) string {
	return "\x1b[1;1H" + strings.Repeat("W", cols+3)
}
