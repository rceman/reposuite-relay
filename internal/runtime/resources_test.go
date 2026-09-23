//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeProcTree builds a /proc-like directory: <pid>/task/<pid>/children
// and <pid>/smaps_rollup.
func fakeProcTree(t *testing.T, root string, tree map[int][]int,
	mem map[int][2]int64) {
	t.Helper()
	for pid, kids := range tree {
		dir := filepath.Join(root, fmt.Sprint(pid), "task", fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var body string
		for _, k := range kids {
			body += fmt.Sprint(k) + " "
		}
		if err := os.WriteFile(filepath.Join(dir, "children"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, m := range mem {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("Rss:               %d kB\nPss:               %d kB\n", m[1], m[0])
		if err := os.WriteFile(filepath.Join(dir, "smaps_rollup"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSampleTreeRootOnly(t *testing.T) {
	root := t.TempDir()
	fakeProcTree(t, root, map[int][]int{100: {}}, map[int][2]int64{100: {1024, 4096}})
	res, ok := SampleTreeResources(root, 100)
	if !ok || res.PSSBytes != 1024*1024 || res.RSSBytes != 4096*1024 || res.ProcessCount != 1 {
		t.Fatalf("root-only: %+v ok=%v", res, ok)
	}
}

func TestSampleTreeNestedDescendants(t *testing.T) {
	root := t.TempDir()
	fakeProcTree(t, root,
		map[int][]int{100: {200, 300}, 200: {400}, 300: {}, 400: {}},
		map[int][2]int64{100: {100, 400}, 200: {200, 800}, 300: {50, 200}, 400: {25, 100}})
	res, ok := SampleTreeResources(root, 100)
	if !ok || res.ProcessCount != 4 {
		t.Fatalf("nested: %+v ok=%v", res, ok)
	}
	if res.PSSBytes != (100+200+50+25)*1024 || res.RSSBytes != (400+800+200+100)*1024 {
		t.Fatalf("aggregate: %+v", res)
	}
}

func TestSampleTreeCycleSafe(t *testing.T) {
	root := t.TempDir()
	// Pathological cycle: 100 → 200 → 100 must terminate once.
	fakeProcTree(t, root,
		map[int][]int{100: {200}, 200: {100}},
		map[int][2]int64{100: {100, 100}, 200: {200, 200}})
	res, ok := SampleTreeResources(root, 100)
	if !ok || res.ProcessCount != 2 || res.PSSBytes != 300*1024 {
		t.Fatalf("cycle: %+v ok=%v", res, ok)
	}
}

func TestSampleTreePartialAndGone(t *testing.T) {
	root := t.TempDir()
	// Child listed but already gone (no smaps_rollup) — skipped, root ok.
	fakeProcTree(t, root, map[int][]int{100: {999}}, map[int][2]int64{100: {100, 400}})
	res, ok := SampleTreeResources(root, 100)
	if !ok || res.ProcessCount != 1 {
		t.Fatalf("partial: %+v ok=%v", res, ok)
	}
	// Root itself unreadable → unavailable, never "zero".
	if _, ok := SampleTreeResources(root, 4242); ok {
		t.Fatal("missing root must report unavailable")
	}
	// Malformed smaps_rollup → unavailable.
	bad := filepath.Join(root, "555")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "smaps_rollup"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, ok = SampleTreeResources(root, 555)
	if !ok {
		t.Fatal("malformed-but-readable rollup should still sample")
	}
	if res.PSSBytes != 0 || res.RSSBytes != 0 {
		t.Fatalf("malformed values must stay zero, got %+v", res)
	}
}
