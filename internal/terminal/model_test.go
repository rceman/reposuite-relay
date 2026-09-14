package terminal

import (
	"bytes"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
	xterm "github.com/rceman/xterm-go"
)

func newTestModel(t *testing.T, cols, rows int) (*Model, *[][]byte) {
	t.Helper()
	var replies [][]byte
	m := New(Options{
		Cols:       cols,
		Rows:       rows,
		Scrollback: 100,
		Fg:         [3]uint8{0xe5, 0xe5, 0xe5},
		Bg:         [3]uint8{0x1a, 0x1a, 0x1a},
		Reply:      func(b []byte) { replies = append(replies, bytes.Clone(b)) },
	})
	t.Cleanup(m.Dispose)
	return m, &replies
}

func replyContains(replies [][]byte, want string) bool {
	joined := bytes.Join(replies, nil)
	return bytes.Contains(joined, []byte(want))
}

// TestCodexProbeBatch feeds the exact Codex startup probe batch and verifies
// every query receives the correct terminal-generated reply with zero
// viewers attached.
func TestCodexProbeBatch(t *testing.T) {
	m, replies := newTestModel(t, 120, 30)
	if _, err := m.Write([]byte(fixtures.CodexProbes)); err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(*replies, nil))
	t.Logf("replies: %q", joined)

	expects := []struct {
		name string
		want string
	}{
		{"CSI 6n CPR", "\x1b[1;1R"},
		{"OSC 10 fg report", "\x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\"},
		{"OSC 11 bg report", "\x1b]11;rgb:1a1a/1a1a/1a1a\x1b\\"},
		{"CSI ?u kitty flags", "\x1b[?0u"},
		{"CSI c primary DA", "\x1b[?1;2c"},
	}
	for _, e := range expects {
		if !replyContains(*replies, e.want) {
			t.Errorf("%s: reply %q not found in %q", e.name, e.want, joined)
		}
	}
	if len(*replies) != len(expects) {
		t.Errorf("expected %d reply events, got %d: %q", len(expects), len(*replies), joined)
	}
}

// TestQueriesIndividually pins each required query's exact reply bytes.
func TestQueriesIndividually(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"DSR cursor position", "\x1b[6n", "\x1b[4;9R"},
		{"DSR status", "\x1b[5n", "\x1b[0n"},
		{"DA primary", "\x1b[c", "\x1b[?1;2c"},
		{"DA secondary", "\x1b[>c", "\x1b[>0;276;0c"},
		{"kitty keyboard query", "\x1b[?u", "\x1b[?0u"},
		{"OSC 10 query", "\x1b]10;?\x1b\\", "\x1b]10;rgb:e5e5/e5e5/e5e5\x1b\\"},
		{"OSC 11 query", "\x1b]11;?\x1b\\", "\x1b]11;rgb:1a1a/1a1a/1a1a\x1b\\"},
		{"OSC 4 palette query", "\x1b]4;196;?\x1b\\", "\x1b]4;196;rgb:ffff/0000/0000\x1b\\"},
		{"DECRQM bracketed paste", "\x1b[?2004$p", "\x1b[?2004;2$y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, replies := newTestModel(t, 80, 24)
			// Place cursor at a known spot for the CPR expectation.
			m.Term.WriteString("\x1b[4;9H")
			*replies = nil
			m.Term.WriteString(tc.query)
			got := string(bytes.Join(*replies, nil))
			if got != tc.want {
				t.Fatalf("query %q: got %q want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestKittyKeyboardStack exercises push/set/query of kitty flags — Codex
// pushes flags after detecting support.
func TestKittyKeyboardStack(t *testing.T) {
	m, replies := newTestModel(t, 80, 24)
	m.Term.WriteString("\x1b[>1u\x1b[?u")
	got := string(bytes.Join(*replies, nil))
	if got != "\x1b[?1u" {
		t.Fatalf("after push 1: got %q want %q", got, "\x1b[?1u")
	}
	*replies = nil
	m.Term.WriteString("\x1b[>15u\x1b[?u")
	got = string(bytes.Join(*replies, nil))
	if got != "\x1b[?15u" {
		t.Fatalf("after push 15: got %q want %q", got, "\x1b[?15u")
	}
	*replies = nil
	m.Term.WriteString("\x1b[<u\x1b[?u")
	got = string(bytes.Join(*replies, nil))
	if got != "\x1b[?1u" {
		t.Fatalf("after pop: got %q want %q", got, "\x1b[?1u")
	}
}

// TestColorSetQueryRestore verifies palette mutation via OSC 10/4 is stored
// host-side and reported back on query.
func TestColorSetQueryRestore(t *testing.T) {
	m, replies := newTestModel(t, 80, 24)
	m.Term.WriteString("\x1b]10;rgb:12/34/56\x1b\\")
	m.Term.WriteString("\x1b]4;42;#abcdef\x1b\\")
	*replies = nil
	m.Term.WriteString("\x1b]10;?\x1b\\\x1b]4;42;?\x1b\\")
	got := string(bytes.Join(*replies, nil))
	wantFg := "\x1b]10;rgb:1212/3434/5656\x1b\\"
	wantIdx := "\x1b]4;42;rgb:abab/cdcd/efef\x1b\\"
	if !replyContains(*replies, wantFg) || !replyContains(*replies, wantIdx) {
		t.Fatalf("got %q, want both %q and %q", got, wantFg, wantIdx)
	}
}

// TestOnDataFromRealTerminalPath verifies reply emission happens inside a
// single Write call synchronously (required for zero-viewer operation).
func TestOnDataSynchronous(t *testing.T) {
	var got []byte
	m := New(Options{Cols: 10, Rows: 3, Reply: func(b []byte) { got = append(got, b...) }})
	defer m.Dispose()
	m.Write([]byte("\x1b[6n"))
	if len(got) == 0 {
		t.Fatal("expected synchronous reply inside Write")
	}
	if string(got) != "\x1b[1;1R" {
		t.Fatalf("got %q", got)
	}
}

// TestWithoutKittyExtension documents behavior when the extension is off:
// CSI ?u receives no reply (correct unsupported-terminal behavior).
func TestWithoutKittyExtension(t *testing.T) {
	var got []byte
	term := xterm.New(xterm.WithCols(10), xterm.WithRows(3))
	defer term.Dispose()
	term.OnData(func(s string) { got = append(got, s...) })
	term.WriteString("\x1b[?u")
	if len(got) != 0 {
		t.Fatalf("expected silence without kitty ext, got %q", got)
	}
}

// TestQueryClassification ensures every Codex-probe-class query produces at
// least one reply through the model (automatic or host-configured).
func TestQueryClassification(t *testing.T) {
	queries := []string{
		"\x1b[6n", "\x1b[5n", "\x1b[c", "\x1b[>c", "\x1b[?u", "\x1b[?6n",
		"\x1b]10;?\x1b\\", "\x1b]11;?\x1b\\", "\x1b]12;?\x1b\\", "\x1b]4;7;?\x1b\\",
	}
	for _, q := range queries {
		m, replies := newTestModel(t, 80, 24)
		m.Term.WriteString(q)
		if len(*replies) == 0 {
			t.Errorf("query %q produced no reply", q)
		}
	}
}
