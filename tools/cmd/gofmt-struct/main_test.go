package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheckAndWriteModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte("package fixture\n\ntype S struct { A int; B int }\nvar _ = S{A: 1, B: 2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--check", path}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("check mode accepted non-canonical source")
	}
	if err := run([]string{"--write", path}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("write mode: %v", err)
	}
	if err := run([]string{"--check", path}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("check mode after write: %v", err)
	}
}

func TestRunReportsOnlyTheChangedFile(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "canonical.go")
	nonCanonical := filepath.Join(dir, "non_canonical.go")
	if err := os.WriteFile(canonical, []byte("package fixture\n\ntype S struct {\n\tA int\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nonCanonical, []byte("package fixture\n\ntype S struct { A int }\nvar _ = S{A: 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"--check", canonical, nonCanonical}, &out, &strings.Builder{}); err == nil {
		t.Fatal("check mode accepted non-canonical source")
	}
	if strings.Contains(out.String(), canonical) || !strings.Contains(out.String(), nonCanonical) {
		t.Fatalf("reported files = %q", out.String())
	}
}

func TestRunDirectoryModeCoversEveryGoFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package fixture\n\ntype S struct { A int; B int }\nvar _ = S{A: 1, B: 2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--check", dir}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("directory check accepted non-canonical source")
	}
	if err := run([]string{"--write", dir}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--check", dir}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("check after write: %v", err)
	}
}

func TestRunUsesSiblingFilesToLeaveNamedMapUnchanged(t *testing.T) {
	dir := t.TempDir()
	typesPath := filepath.Join(dir, "types.go")
	usePath := filepath.Join(dir, "use.go")
	if err := os.WriteFile(typesPath, []byte("package fixture\n\ntype Alias map[string]int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := []byte("package fixture\n\nvar _ = Alias{\"a\": 1, \"b\": 2}\n")
	if err := os.WriteFile(usePath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--check", usePath}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("cross-file named map failed check: %v", err)
	}
	if err := run([]string{"--write", usePath}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("cross-file named map failed write: %v", err)
	}
	got, err := os.ReadFile(usePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("cross-file named map changed:\n%s", got)
	}
}

func TestRunRequiresExactlyOneMode(t *testing.T) {
	if err := run(nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("missing mode accepted")
	}
	if err := run([]string{"--check", "--write", "x.go"}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("both modes accepted")
	}
}
