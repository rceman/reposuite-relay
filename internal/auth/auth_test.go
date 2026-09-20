package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestLoadOversizeFails: an oversized credential fails closed — a
// truncated prefix is never normalized into a valid token.
func TestLoadOversizeFails(t *testing.T) {
	tok, _ := Generate()
	dir := t.TempDir()
	for name, raw := range map[string]string{
		// Pathological: valid token + excessive CR/LF — oversized file.
		"huge-newlines": tok + strings.Repeat("\r\n", maxFile),
		"padding":       tok + "\n" + strings.Repeat(" ", maxFile),
		"two-tokens":    tok + "\n" + tok,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%s: oversized credential must fail closed", name)
		}
		if _, err := Ensure(path); err == nil {
			t.Fatalf("%s: Ensure must fail, not replace", name)
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatalf("%s: oversized credential was rewritten", name)
		}
	}
}

// TestLoadFileMode: exactly 0600 — any other mode fails closed, and the
// file is never chmod-repaired or rewritten.
func TestLoadFileMode(t *testing.T) {
	tok, _ := Generate()
	dir := t.TempDir()
	path := filepath.Join(dir, "api.token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || got != tok {
		t.Fatalf("0600 token must load: %v", err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o666, 0o604, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("mode %o credential must fail closed", mode)
		}
		if _, err := Ensure(path); err == nil {
			t.Fatalf("mode %o: Ensure must fail, not replace", mode)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != mode {
			t.Fatalf("mode %o was silently repaired", mode)
		}
		after, _ := os.ReadFile(path)
		if string(after) != tok+"\n" {
			t.Fatalf("mode %o: token bytes changed", mode)
		}
	}
}
