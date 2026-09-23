package presentation

import (
	"strings"
	"testing"
)

func prj(name, root string) Project {
	return Project{
		ID:   "prj_" + strings.Repeat("0", 32),
		Name: name,
		Root: root,
	}
}

func TestMatchExactDescendantBoundary(t *testing.T) {
	cat := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{
			{ID: "prj_" + strings.Repeat("a", 32), Name: "foo", Root: "/work/foo"},
		},
	}
	cases := []struct {
		cwd   string
		match bool
	}{
		{"/work/foo", true},          // exact
		{"/work/foo/", true},         // trailing slash cleans to exact
		{"/work/foo/src", true},      // descendant
		{"/work/foo/src/deep", true}, // deeper descendant
		{"/work/foobar", false},      // component boundary — NOT a prefix match
		{"/work/fo", false},          // ancestor
		{"/other", false},            // unrelated
	}
	for _, tc := range cases {
		p, ok := Match(cat, tc.cwd)
		if ok != tc.match {
			t.Fatalf("match(%q) = %v, want %v", tc.cwd, ok, tc.match)
		}
		if ok && p.Root != "/work/foo" {
			t.Fatalf("match(%q) = %q, want /work/foo", tc.cwd, p.Root)
		}
	}
}

func TestMatchLongestRootWins(t *testing.T) {
	cat := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{
			{ID: "prj_" + strings.Repeat("a", 32), Name: "outer", Root: "/work"},
			{ID: "prj_" + strings.Repeat("b", 32), Name: "inner", Root: "/work/relay"},
			{ID: "prj_" + strings.Repeat("c", 32), Name: "deep", Root: "/work/relay/internal"},
		},
	}
	p, ok := Match(cat, "/work/relay/internal/daemon")
	if !ok || p.ID != cat.Projects[2].ID {
		t.Fatalf("deep match = %+v, want the most-specific project", p)
	}
	p, ok = Match(cat, "/work/relay/web")
	if !ok || p.ID != cat.Projects[1].ID {
		t.Fatalf("mid match = %+v, want /work/relay", p)
	}
	p, ok = Match(cat, "/work/other")
	if !ok || p.ID != cat.Projects[0].ID {
		t.Fatalf("outer match = %+v, want /work", p)
	}
	if _, ok := Match(cat, "/home/user"); ok {
		t.Fatal("unmatched cwd must report no match")
	}
}

func TestMatchIsPure(t *testing.T) {
	// No filesystem requirement: nonexistent roots/cwds match lexically.
	cat := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{
			prj("offline", "/mnt/offline/project"),
		},
	}
	if p, ok := Match(cat, "/mnt/offline/project/src"); !ok || p.Name != "offline" {
		t.Fatal("matching must be lexical, never filesystem-dependent")
	}
}
