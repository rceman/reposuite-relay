package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateAndValid(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(tok) {
		t.Fatalf("generated token %q fails Valid", tok)
	}
	for _, bad := range []string{"", "abc", tok + "0", tok[:63], "G" + tok[1:], tok + "\n"} {
		if Valid(bad) {
			t.Fatalf("%q must not be valid", bad)
		}
	}
}

func TestEnsureCreatesThenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.token")
	tok, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(tok) {
		t.Fatal("created token malformed")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %o, want 0600", fi.Mode().Perm())
	}
	// A second Ensure loads the SAME canonical token — never re-mints.
	tok2, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok {
		t.Fatal("Ensure re-minted an existing token")
	}
	loaded, err := Load(path)
	if err != nil || loaded != tok {
		t.Fatalf("Load = %q, %v", loaded, err)
	}
}

func TestLoadMissingAndMalformed(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "api.token"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
	for _, raw := range []string{"", "short", "not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-not-hex-n", "a" + string(make([]byte, 0))} {
		dir := t.TempDir()
		path := filepath.Join(dir, "api.token")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("malformed %q must fail closed, got %v", raw, err)
		}
		// Malformed credentials are never silently replaced.
		if _, err := Ensure(path); err == nil {
			t.Fatalf("Ensure on malformed %q must fail, not replace", raw)
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatalf("malformed token %q was rewritten", raw)
		}
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.token")
	tok, _ := Generate()
	if err := os.WriteFile(target, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "api.token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink token must fail closed")
	}
}
