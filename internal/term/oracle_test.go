package term

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/fixtures"
)

// Oracle parity tests: feed identical byte streams to @xterm/headless (the
// oracle, spike-only) and to xterm-go, then diff canonical fingerprints.
// The oracle lives in spike-oracle/ and is NOT a production dependency.

const oracleDir = "../../spike-oracle"

func oracleAvailable(t *testing.T) string {
	t.Helper()
	script := filepath.Join(oracleDir, "oracle.mjs")
	if _, err := os.Stat(script); err != nil {
		t.Skip("spike-oracle/oracle.mjs missing")
	}
	if _, err := os.Stat(filepath.Join(oracleDir, "node_modules", "@xterm", "headless")); err != nil {
		t.Skip("spike-oracle node_modules missing; run npm install in spike-oracle/")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	return script
}

// runOracle feeds fixtureFile through @xterm/headless and returns the
// normalized fingerprint map.
func runOracle(t *testing.T, script, fixtureFile string, cols, rows int, extra ...string) map[string]any {
	t.Helper()
	args := []string{script, fixtureFile, "--cols", fmt.Sprint(cols), "--rows", fmt.Sprint(rows)}
	args = append(args, extra...)
	out, err := exec.Command("node", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("oracle failed: %v\n%s", err, out)
	}
	var fp map[string]any
	if err := json.Unmarshal(out, &fp); err != nil {
		t.Fatalf("oracle output not JSON: %v\n%s", err, out)
	}
	return normalize(fp)
}

var mouseEncRe = regexp.MustCompile(` mousenc=\S+`)

// normalize drops fields the JS public API cannot provide (marked "n/a" on
// the oracle side; Go-only otherwise) and normalizes the modes string.
// Scroll-region, cursor-visibility, and current-pen remain Go-verified by
// the non-oracle tests.
func normalize(fp map[string]any) map[string]any {
	for k, v := range fp {
		if s, ok := v.(string); ok && s == "n/a" {
			delete(fp, k)
		}
	}
	for _, k := range []string{"curAttr", "cursorHidden", "scrollTop", "scrollBottom"} {
		delete(fp, k)
	}
	if m, ok := fp["modes"].(string); ok {
		fp["modes"] = mouseEncRe.ReplaceAllString(m, "")
	}
	return fp
}

func goFingerprint(t *testing.T, stream []byte, cols, rows int, resizeSeq [][2]int, snapshot bool) map[string]any {
	t.Helper()
	m := New(Options{Cols: cols, Rows: rows, Scrollback: 1000, Reply: func([]byte) {}})
	defer m.Dispose()
	m.Write(stream)
	for _, rs := range resizeSeq {
		m.Term.Resize(rs[0], rs[1])
	}
	target := m.Term
	var dst *Model
	if snapshot {
		dst = New(Options{Cols: cols, Rows: rows, Scrollback: 1000, Reply: func([]byte) {}})
		defer dst.Dispose()
		dst.Write(append([]byte(liveReset), m.Snapshot(0)...))
		target = dst.Term
	}
	var fp map[string]any
	if err := json.Unmarshal([]byte(TakeFingerprint(target).JSON()), &fp); err != nil {
		t.Fatal(err)
	}
	return normalize(fp)
}

func diffMaps(t *testing.T, want, got map[string]any) []string {
	t.Helper()
	var diffs []string
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s still present in go, absent in oracle", k))
			continue
		}
		if k == "cells" {
			diffs = append(diffs, diffCells(wv, gv)...)
			continue
		}
		if fmt.Sprint(wv) != fmt.Sprint(gv) {
			diffs = append(diffs, fmt.Sprintf("%s: go=%v oracle=%v", k, gv, wv))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s present in oracle only", k))
		}
	}
	return diffs
}

func diffCells(wv, gv any) []string {
	wr, _ := wv.([]any)
	gr, _ := gv.([]any)
	var diffs []string
	n := min(len(wr), len(gr))
	for y := 0; y < n; y++ {
		wl, _ := wr[y].([]any)
		gl, _ := gr[y].([]any)
		m := min(len(wl), len(gl))
		for x := 0; x < m; x++ {
			if fmt.Sprint(wl[x]) != fmt.Sprint(gl[x]) {
				diffs = append(diffs, fmt.Sprintf("cell[%d,%d]: go=%v oracle=%v", y, x, gl[x], wl[x]))
			}
		}
		if len(wl) != len(gl) {
			diffs = append(diffs, fmt.Sprintf("cell row %d length %d != %d", y, len(gl), len(wl)))
		}
	}
	if len(wr) != len(gr) {
		diffs = append(diffs, fmt.Sprintf("cells rows %d != %d", len(gr), len(wr)))
	}
	return diffs
}

// compareWithOracle runs one parity case: identical stream to both engines.
func compareWithOracle(t *testing.T, name string, stream []byte, cols, rows int, extra ...string) {
	t.Helper()
	script := oracleAvailable(t)
	fx := filepath.Join(t.TempDir(), "fixture.raw")
	if err := os.WriteFile(fx, stream, 0o644); err != nil {
		t.Fatal(err)
	}
	oracle := runOracle(t, script, fx, cols, rows, extra...)

	var resizeSeq [][2]int
	snapshot := false
	for i := 0; i < len(extra); i++ {
		if extra[i] == "--resize" {
			for _, p := range strings.Split(extra[i+1], ",") {
				var c, r int
				fmt.Sscanf(p, "%d:%d", &c, &r)
				resizeSeq = append(resizeSeq, [2]int{c, r})
			}
			i++
		}
		if extra[i] == "--snapshot" {
			snapshot = true
		}
	}
	goFp := goFingerprint(t, stream, cols, rows, resizeSeq, snapshot)

	if d := diffMaps(t, oracle, goFp); len(d) > 0 {
		t.Fatalf("%s: %d diffs vs oracle:\n%s", name, len(d), strings.Join(d[:min(12, len(d))], "\n"))
	}
}

func TestOracleParity(t *testing.T) {
	oracleAvailable(t)
	cases := []struct {
		name   string
		stream []byte
		cols   int
		rows   int
		extra  []string
	}{
		{"style-tour", []byte(fixtures.StyleTour), 120, 24, nil},
		{"fullwidth-bg", []byte(fixtures.FullWidthBackground(128)), 128, 24, nil},
		{"unicode", []byte(fixtures.Unicode), 80, 24, nil},
		{"alt-buffer", []byte(fixtures.AltBufferUI), 80, 24, nil},
		{"bce", []byte(fixtures.BCE), 128, 24, nil},
		{"wrap-reflow-narrow-wide", []byte(fixtures.WrapLine(40) + "\x1b[6;1H"), 40, 10, []string{"--resize", "20:10,80:10"}},
		{"wrap-cursor-line-quirk", []byte(fixtures.WrapLine(40)), 40, 10, []string{"--resize", "20:10"}},
		{"snapshot-style-tour", []byte(fixtures.StyleTour), 80, 6, []string{"--snapshot", "0"}},
		{"snapshot-fullwidth-bg", []byte(fixtures.FullWidthBackground(128)), 128, 5, []string{"--snapshot", "0"}},
		{"snapshot-alt-buffer", []byte(fixtures.AltBufferUI), 30, 4, []string{"--snapshot", "0"}},
		{"snapshot-unicode", []byte(fixtures.Unicode), 20, 3, []string{"--snapshot", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareWithOracle(t, tc.name, tc.stream, tc.cols, tc.rows, tc.extra...)
		})
	}
}
