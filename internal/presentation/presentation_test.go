package presentation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCat writes a catalog fixture file with the standard private mode.
func writeCat(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func validJSON() string {
	return `{"schemaVersion":1,"projects":[{"id":"prj_` +
		strings.Repeat("a", 32) + `","name":"Relay","root":"/work/relay"}]}`
}

func TestLoadMissingFileIsEmptyCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	cat, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cat.SchemaVersion != SchemaVersion || len(cat.Projects) != 0 {
		t.Fatalf("missing file = %+v, want empty schema-1 catalog", cat)
	}
	// Reads never create the file.
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("a read must never create presentation.json")
	}
}

func TestLoadValidRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presentation.json")
	in := Catalog{
		SchemaVersion: SchemaVersion,
		Projects: []Project{
			{ID: "prj_" + strings.Repeat("a", 32), Name: "Relay", Root: "/work/relay"},
		},
	}
	if err := Commit(path, in); err != nil {
		t.Fatal(err)
	}
	cat, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Projects) != 1 || cat.Projects[0].ID != in.Projects[0].ID {
		t.Fatalf("round trip = %+v", cat.Projects)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("committed mode %o, want 0600", fi.Mode().Perm())
	}
}

func TestLoadStrictRejections(t *testing.T) {
	dir := t.TempDir()
	good := "prj_" + strings.Repeat("a", 32)
	cases := []struct {
		name    string
		content string
	}{
		{"malformed JSON", `{`},
		{"trailing content", validJSON() + ` {}`},
		{"unknown field", `{"schemaVersion":1,"projects":[],"extra":1}`},
		{"bad schema version", `{"schemaVersion":2,"projects":[]}`},
		{"bad project id", `{"schemaVersion":1,"projects":[{"id":"x","name":"a","root":"/a"}]}`},
		{"empty name", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"  ","root":"/a"}]}`},
		{"control-char name", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"a\n","root":"/a"}]}`},
		{"relative root", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"a","root":"work/a"}]}`},
		{"unclean root", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"a","root":"/a/../a"}]}`},
		{"duplicate id", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"a","root":"/a"},{"id":"` + good + `","name":"b","root":"/b"}]}`},
		{"duplicate root", `{"schemaVersion":1,"projects":[{"id":"` + good + `","name":"a","root":"/a"},{"id":"prj_` + strings.Repeat("b", 32) + `","name":"b","root":"/a"}]}`},
	}
	for _, tc := range cases {
		path := filepath.Join(dir, tc.name+".json")
		writeCat(t, path, tc.content, 0o600)
		if _, err := Load(path); err == nil {
			t.Fatalf("%s: malformed config accepted", tc.name)
		}
		// Malformed authority is never rewritten.
		raw, _ := os.ReadFile(path)
		if string(raw) != tc.content {
			t.Fatalf("%s: malformed file was rewritten", tc.name)
		}
	}
}

func TestLoadRejectsNonRegularAndExposed(t *testing.T) {
	dir := t.TempDir()
	// Symlink is not a regular file.
	target := filepath.Join(dir, "target.json")
	writeCat(t, target, validJSON(), 0o600)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink accepted as presentation config")
	}
	// Directory is not a regular file.
	if _, err := Load(dir); err == nil {
		t.Fatal("directory accepted as presentation config")
	}
	// Group/other permissions fail closed — never silently repaired.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o400} {
		path := filepath.Join(dir, "mode.json")
		writeCat(t, path, validJSON(), mode)
		if _, err := Load(path); err == nil {
			t.Fatalf("mode %o accepted — want 0600 only", mode)
		}
	}
	// Oversized file fails the bound.
	big := filepath.Join(dir, "big.json")
	good := validJSON()
	writeCat(t, big, good[:len(good)-1]+strings.Repeat(" ", MaxFile), 0o600)
	if _, err := Load(big); err == nil {
		t.Fatal("oversized presentation config accepted")
	}
}

func TestCatalogTooManyProjects(t *testing.T) {
	c := Catalog{SchemaVersion: SchemaVersion}
	for i := 0; i <= MaxProjects; i++ {
		c.Projects = append(c.Projects, Project{
			ID:   "prj_" + strings.Repeat(string(rune('a'+i%26)), 31) + string(rune('a'+i/26)),
			Name: "p",
			Root: "/r" + strings.Repeat("x", i%3) + strings.Repeat("y", i),
		})
	}
	if err := c.Validate(); err == nil {
		t.Fatal("catalog over the project bound accepted")
	}
}
