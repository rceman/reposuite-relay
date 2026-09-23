// Durable size bound: the write path enforces the SAME MaxFile Load
// enforces — a successful commit must never persist a catalog the next
// startup would refuse.
package presentation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitOversizeBound: the write path enforces the SAME MaxFile
// bound Load enforces — a serialized catalog over the bound fails
// pre-commit with disk untouched.
func TestCommitOversizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	id := "prj_" + strings.Repeat("0", 32)

	// The canonical byte length is linear in the root length — calibrate
	// the constant overhead with one marshal instead of looping.
	size := func(c Catalog) int {
		raw, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return len(raw) + 1 // trailing newline is part of the durable file
	}
	probe := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{{
			ID: id, Name: "p", Root: "/w/",
		}},
	}
	overhead := size(probe) - len("/w/")

	// Land the canonical file just under MaxFile — assert real bytes.
	fit := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{{
			ID: id, Name: "p", Root: "/w/" + strings.Repeat("x", MaxFile-overhead-200),
		}},
	}
	under := size(fit)
	if under > MaxFile || under < MaxFile-1024 {
		t.Fatalf("fixture miscalibrated: %d", under)
	}
	if err := Commit(path, fit); err != nil {
		t.Fatalf("catalog of %d bytes must commit: %v", under, err)
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != under {
		t.Fatalf("committed %d bytes, marshaled %d", len(committed), under)
	}

	// Grow the root past the bound: pre-commit failure, byte-identical
	// canonical file, no PostCommitError, no temp leak.
	fit.Projects[0].Root += strings.Repeat("y", 512)
	if size(fit) <= MaxFile {
		t.Fatalf("fixture not oversized: %d <= %d", size(fit), MaxFile)
	}
	err = Commit(path, fit)
	if err == nil {
		t.Fatalf("catalog of %d bytes must not commit", size(fit))
	}
	if IsPostCommit(err) {
		t.Fatal("oversize is pre-commit, never post-commit")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(committed) {
		t.Fatal("canonical file changed on a pre-commit rejection")
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("committed catalog must still load: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("temp leak: %v", ents)
	}
}

// TestManagerOversizeAccumulation: individually valid mutations must
// never accumulate a catalog beyond MaxFile — the fill mutation that
// crosses the bound is a clean pre-commit error.
func TestManagerOversizeAccumulation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presentation.json")
	mgr, err := NewManager(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each ~2 KiB root accumulates toward the 256 KiB file bound.
	var n int
	for {
		root := fmt.Sprintf("/w/%s%d", strings.Repeat("x", 2000), n)
		_, err := mgr.Create("p", root)
		if err != nil {
			break // the crossing mutation fails
		}
		n++
		if n > MaxProjects {
			t.Fatal("project count bound reached before size bound")
		}
	}
	if n == 0 {
		t.Fatal("no projects fit under MaxFile — fixture is broken")
	}
	want := mgr.Snapshot()
	if len(want.Projects) != n {
		t.Fatalf("snapshot mutated on rejection: %d != %d", len(want.Projects), n)
	}
	disk := catFile(t, path)
	if len(disk.Projects) != n {
		t.Fatalf("disk catalog mutated on rejection: %d != %d", len(disk.Projects), n)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("committed catalog must load on next generation: %v", err)
	}
	// No temp-file residue from the rejected commit.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".presentation.json.tmp-") {
			t.Fatalf("temp leak: %s", e.Name())
		}
	}
}
