package presentation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// catFile loads the durable file directly — the ground truth for what a
// commit actually left on disk.
func catFile(t *testing.T, path string) Catalog {
	t.Helper()
	cat, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestCommitPreCommitFailures(t *testing.T) {
	// A committed baseline the failed mutations must never disturb.
	path := filepath.Join(t.TempDir(), "presentation.json")
	m, err := NewManager(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := m.Create("base", "/work/base")
	if err != nil {
		t.Fatal(err)
	}
	baseline := catFile(t, path)
	if len(baseline.Projects) != 1 || baseline.Projects[0].ID != base.ID {
		t.Fatalf("baseline = %+v", baseline.Projects)
	}

	injected := errors.New("injected commit failure")
	preCommit := []struct {
		name  string
		hooks *Hooks
	}{
		{"write", &Hooks{
			WriteTemp: func(f *os.File, b []byte) (int, error) {
				return 0, injected
			},
		}},
		{"fsync", &Hooks{
			SyncTemp: func(f *os.File) error {
				return injected
			},
		}},
		{"rename", &Hooks{
			Rename: func(o, n string) error {
				return injected
			},
		}},
	}
	for _, tc := range preCommit {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "presentation.json")
			if err := Commit(p, baseline); err != nil {
				t.Fatal(err)
			}
			mgr, err := NewManager(p, tc.hooks)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mgr.Create("two", "/work/two"); err == nil {
				t.Fatal("pre-commit fault must fail the mutation")
			}
			// Old canonical file remains; memory remains old.
			got := catFile(t, p)
			if len(got.Projects) != 1 {
				t.Fatalf("canonical catalog = %d projects, want 1", len(got.Projects))
			}
			if len(mgr.Snapshot().Projects) != 1 {
				t.Fatal("in-memory catalog diverged on a pre-commit failure")
			}
			// No temp garbage.
			ents, _ := os.ReadDir(filepath.Dir(p))
			for _, e := range ents {
				if strings.Contains(e.Name(), ".tmp-") {
					t.Fatalf("staged temp leaked: %s", e.Name())
				}
			}
		})
	}
}

func TestCommitPostCommitConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	injected := errors.New("injected dirsync failure")
	mgr, err := NewManager(path, &Hooks{SyncDir: func(dir string) error {
		return injected
	}})
	if err != nil {
		t.Fatal(err)
	}
	prj, err := mgr.Create("relay", "/work/relay")
	if err == nil {
		t.Fatal("post-commit fault must surface")
	}
	if !IsPostCommit(err) {
		t.Fatalf("err = %v, want *PostCommitError", err)
	}
	// The rename committed: disk has the new catalog AND memory converged
	// to it — never disk-new/memory-old.
	if got := catFile(t, path); len(got.Projects) != 1 || got.Projects[0].ID != prj.ID {
		t.Fatalf("canonical file = %+v, want the committed project", got.Projects)
	}
	if got := mgr.Snapshot(); len(got.Projects) != 1 || got.Projects[0].ID != prj.ID {
		t.Fatalf("in-memory catalog did not converge to committed content")
	}
}

func TestConcurrentMutationsSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	mgr, err := NewManager(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 16 racing creates with distinct roots: no lost update, no duplicate
	// IDs/roots, and the durable file validates against what memory holds.
	const n = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p, err := mgr.Create("worker", "/work/"+strings.Repeat("p", i+1))
			if err != nil {
				errs <- err
				return
			}
			ids <- p.ID
		}(i)
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent create failed: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate project ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("created %d projects, want %d", len(seen), n)
	}
	disk := catFile(t, path)
	mem := mgr.Snapshot()
	if len(disk.Projects) != n || len(mem.Projects) != n {
		t.Fatalf("disk=%d mem=%d, want %d each", len(disk.Projects), len(mem.Projects), n)
	}
	for i := range disk.Projects {
		if disk.Projects[i] != mem.Projects[i] {
			t.Fatalf("project %d diverged: disk %+v mem %+v", i, disk.Projects[i], mem.Projects[i])
		}
	}
	// Canonical output order: name, root, id — deterministic.
	for i := 1; i < len(disk.Projects); i++ {
		a, b := disk.Projects[i-1], disk.Projects[i]
		if a.Name > b.Name || (a.Name == b.Name && a.Root > b.Root) ||
			(a.Name == b.Name && a.Root == b.Root && a.ID > b.ID) {
			t.Fatalf("catalog order violated at %d: %q vs %q", i, a.Root, b.Root)
		}
	}
}

func TestUpdateAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	mgr, err := NewManager(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := mgr.Create("  spaced name ", "/work/a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "spaced name" {
		t.Fatalf("name not normalized: %q", a.Name)
	}
	b, err := mgr.Create("other", "/work/b")
	if err != nil {
		t.Fatal(err)
	}
	// Root move colliding with another project's root is a conflict.
	if _, err := mgr.Update(a.ID, "", "/work/b/", false, true); !errors.Is(err, ErrRootConflict) {
		t.Fatalf("root move to duplicate root = %v, want ErrRootConflict", err)
	}
	// Rename preserves the ID; nested-root move is legal.
	upd, err := mgr.Update(a.ID, "renamed", "/work/b/inner", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if upd.ID != a.ID || upd.Name != "renamed" || upd.Root != "/work/b/inner" {
		t.Fatalf("update = %+v", upd)
	}
	if _, err := mgr.Update("prj_missing", "x", "", true, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown = %v, want ErrNotFound", err)
	}
	if err := mgr.Delete("prj_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown = %v, want ErrNotFound", err)
	}
	if err := mgr.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Match("/work/b/inner/sub"); !ok || func() bool {
		p, _ := mgr.Match("/work/b/inner/sub")
		return p.ID != b.ID // falls through to the less-specific /work/b
	}() {
		t.Fatal("deleted project's sessions must fall through to /work/b")
	}
}
