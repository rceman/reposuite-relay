// Machine token rotation: commit-point semantics under injected faults —
// pre-commit failures leave the old credential canonical and expose no
// new token; a post-commit dirsync failure still commits the new
// credential and reports durability unconfirmed.
package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedToken(t *testing.T) (dir, path, old string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "api.token")
	var err error
	old, err = Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(old) {
		t.Fatal("seed token malformed")
	}
	return dir, path, old
}

func TestRotateClean(t *testing.T) {
	dir, path, old := seedToken(t)
	tok, err := Rotate(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(tok) || tok == old {
		t.Fatal("rotate did not mint a fresh valid token")
	}
	got, err := Load(path)
	if err != nil || got != tok {
		t.Fatalf("canonical file does not carry the new token: %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %o, want 0600", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp leak: %v", ents)
	}
}

// Pre-commit faults: no new token is exposed, the old file is
// byte-identical, no temp leak.
func TestRotatePreCommitFailures(t *testing.T) {
	injected := errors.New("injected")
	cases := []struct {
		name  string
		hooks *Hooks
	}{
		{"write", &Hooks{
			WriteTemp: func(f *os.File, b []byte) (int, error) {
				return 0, injected
			},
		}},
		{"sync", &Hooks{
			SyncTemp: func(f *os.File) error { return injected },
		}},
		{"rename", &Hooks{
			Rename: func(o, n string) error { return injected },
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, path, old := seedToken(t)
			before, _ := os.ReadFile(path)
			tok, err := Rotate(path, tc.hooks)
			if err == nil {
				t.Fatal("injected fault must fail rotation")
			}
			if IsPostCommit(err) {
				t.Fatal("pre-commit fault classified as post-commit")
			}
			if tok != "" {
				t.Fatal("pre-commit failure must not expose a token")
			}
			if errors.Is(err, injected) || strings.Contains(err.Error(), "injected") {
				// wrapped or direct is fine
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("canonical credential changed on pre-commit failure")
			}
			if got, _ := Load(path); got != old {
				t.Fatal("old token must remain canonical")
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 1 {
				t.Fatalf("temp leak: %v", ents)
			}
		})
	}
}

// Post-commit: rename already committed — the new token IS canonical,
// durability is unconfirmed, and the caller still receives it.
func TestRotatePostCommitDirsync(t *testing.T) {
	_, path, old := seedToken(t)
	hooks := &Hooks{SyncDir: func(dir string) error {
		return errors.New("injected dirsync")
	}}
	tok, err := Rotate(path, hooks)
	if err == nil {
		t.Fatal("post-commit fault must surface")
	}
	if !IsPostCommit(err) {
		t.Fatalf("dirsync failure is post-commit, got %v", err)
	}
	if !Valid(tok) || tok == old {
		t.Fatal("committed token must be returned even on post-commit error")
	}
	got, lerr := Load(path)
	if lerr != nil || got != tok {
		t.Fatal("canonical file must carry the committed token")
	}
	// The error must never carry credential material.
	if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), old) {
		t.Fatal("token leaked into error text")
	}
}
