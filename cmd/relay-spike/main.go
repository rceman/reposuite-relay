// Command relay-spike is the throwaway spike harness. It wires a PTY child
// to the Go headless terminal model in both directions and can capture a
// child's raw output stream for fixture use.
//
// Usage:
//
//	relay-spike run [-cols N] [-rows N] [-seconds S] -- <cmd> [args...]
//	relay-spike capture [-seconds S] [-o file] -- <cmd> [args...]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rceman/reposuite-relay/internal/ptyx"
	"github.com/rceman/reposuite-relay/internal/term"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relay-spike <run|capture> [flags] -- <cmd> [args...]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	// Split our flags from the child command at "--".
	childAt := len(args)
	for i, a := range args {
		if a == "--" {
			childAt = i
			break
		}
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cols := fs.Int("cols", 120, "terminal columns")
	rows := fs.Int("rows", 30, "terminal rows")
	secs := fs.Float64("seconds", 3, "how long to let the child run")
	out := fs.String("o", "", "capture output file")
	fs.Parse(args[:childAt])
	if childAt >= len(args) || len(args[childAt+1:]) == 0 {
		fmt.Fprintln(os.Stderr, "missing child command after --")
		os.Exit(2)
	}
	child := args[childAt+1:]

	switch cmd {
	case "run":
		os.Exit(runHeadless(child, *cols, *rows, *secs))
	case "capture":
		os.Exit(capture(child, *cols, *rows, *secs, *out))
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", cmd)
		os.Exit(2)
	}
}

// newModel wires a headless terminal to the process: terminal replies go
// back to the child via the PTY master.
func newModel(p *ptyx.Process, cols, rows int) *term.Model {
	return term.New(term.Options{
		Cols:       cols,
		Rows:       rows,
		Scrollback: 1000,
		Fg:         [3]uint8{0xe5, 0xe5, 0xe5},
		Bg:         [3]uint8{0x1a, 0x1a, 0x1a},
		Reply: func(b []byte) {
			// Terminal-generated response -> child stdin.
			_, _ = p.Write(b)
		},
	})
}

func runHeadless(child []string, cols, rows int, secs float64) int {
	p, err := ptyx.Start(child[0], child[1:], uint16(cols), uint16(rows), nil, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pty start:", err)
		return 1
	}
	defer p.Close()

	m := newModel(p, cols, rows)
	defer m.Dispose()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- p.Pump(func(b []byte) { _, _ = m.Write(b) }) }()

	time.Sleep(time.Duration(secs * float64(time.Second)))

	fp := term.TakeFingerprint(m.Term)
	fmt.Printf("== viewport %dx%d alt=%v cursor=%d,%d ==\n",
		fp.Cols, fp.Rows, fp.AltActive, fp.CursorX, fp.CursorY)
	fmt.Println(m.Term.String())
	fmt.Printf("== snapshot bytes: %d ==\n", len(m.Snapshot(0)))

	_ = p.Close()
	<-pumpDone
	return 0
}

func capture(child []string, cols, rows int, secs float64, out string) int {
	p, err := ptyx.Start(child[0], child[1:], uint16(cols), uint16(rows), nil, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pty start:", err)
		return 1
	}
	defer p.Close()

	f := os.Stdout
	if out != "" {
		f, err = os.Create(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			return 1
		}
		defer f.Close()
	}

	pumpDone := make(chan error, 1)
	go func() {
		pumpDone <- p.Pump(func(b []byte) { _, _ = f.Write(b) })
	}()

	time.Sleep(time.Duration(secs * float64(time.Second)))
	_ = p.Close()
	<-pumpDone
	return 0
}
