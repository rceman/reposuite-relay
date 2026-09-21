package adminauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "admin.json")
}

func testCreds(t *testing.T) Credentials {
	t.Helper()
	hash, err := Hash("a-valid-test-password", TestParams)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{
		SchemaVersion: SchemaVersion,
		Username:      "admin",
		PasswordHash:  hash,
	}
}

func writeRaw(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialMissingIsSetupMode(t *testing.T) {
	path := credPath(t)
	_, err := Load(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing admin.json: err %v, want ErrNotExist", err)
	}
}

func TestCredentialCommitOnceRoundTrip(t *testing.T) {
	path := credPath(t)
	creds := testCreds(t)
	created, err := CommitOnce(path, creds)
	if err != nil || !created {
		t.Fatalf("CommitOnce: created=%v err=%v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("admin.json mode %o, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "a-valid-test-password") {
		t.Fatal("plaintext password persisted")
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Username != "admin" || loaded.PasswordHash != creds.PasswordHash {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	// Second commit never overwrites.
	other := testCreds(t)
	other.Username = "other"
	created, err = CommitOnce(path, other)
	if err != nil || created {
		t.Fatalf("second CommitOnce: created=%v err=%v", created, err)
	}
	reloaded, _ := Load(path)
	if reloaded.Username != "admin" {
		t.Fatal("second commit overwrote the credential")
	}
}

// TestCommitOnceOutcomes proves the exact commit contract: pre-commit
// failure reports nothing committed; post-commit failures report
// committed-with-uncertainty and leave the canonical file readable.
func TestCommitOnceOutcomes(t *testing.T) {
	creds := testCreds(t)

	// Pre-commit failure: parent dir absent → temp cannot be staged.
	t.Run("pre-commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing-dir", "admin.json")
		created, err := CommitOnceWith(path, creds, nil)
		if created || err == nil {
			t.Fatalf("pre-commit: created=%v err=%v", created, err)
		}
		var pce *PostCommitError
		if errors.As(err, &pce) {
			t.Fatal("pre-commit failure reported as post-commit")
		}
	})

	// Post-commit dirsync failure: committed, typed error, file loads.
	t.Run("post-commit dirsync", func(t *testing.T) {
		path := credPath(t)
		created, err := CommitOnceWith(path, creds, &Hooks{
			SyncDir: func(string) error { return errors.New("injected dirsync failure") },
		})
		if !created {
			t.Fatal("post-commit failure must still report created=true")
		}
		var pce *PostCommitError
		if !errors.As(err, &pce) || pce.Op != "dirsync" {
			t.Fatalf("want *PostCommitError{dirsync}, got %v", err)
		}
		loaded, err := Load(path)
		if err != nil || loaded.Username != "admin" {
			t.Fatalf("committed credential unreadable: %v", err)
		}
	})

	// Post-commit cleanup failure: committed, surfaced, file loads.
	t.Run("post-commit cleanup", func(t *testing.T) {
		path := credPath(t)
		created, err := CommitOnceWith(path, creds, &Hooks{
			Remove: func(string) error { return errors.New("injected cleanup failure") },
		})
		if !created {
			t.Fatal("cleanup failure must still report created=true")
		}
		var pce *PostCommitError
		if !errors.As(err, &pce) || pce.Op != "cleanup" {
			t.Fatalf("want *PostCommitError{cleanup}, got %v", err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("committed credential unreadable: %v", err)
		}
	})

	// Link failure (non-EEXIST) is a pre-commit failure.
	t.Run("link failure", func(t *testing.T) {
		path := credPath(t)
		created, err := CommitOnceWith(path, creds, &Hooks{
			Link: func(string, string) error { return errors.New("injected link failure") },
		})
		if created || err == nil {
			t.Fatalf("link failure: created=%v err=%v", created, err)
		}
		var pce *PostCommitError
		if errors.As(err, &pce) {
			t.Fatal("link failure reported as post-commit")
		}
		if _, err := Load(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed commit left a credential file")
		}
	})
}

func TestCredentialStrictLoad(t *testing.T) {
	creds := testCreds(t)
	validPath := credPath(t)
	valid, err := CommitOnce(validPath, creds)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("setup commit failed")
	}
	validRaw, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"malformed JSON":    `{"schemaVersion":1,"username":"admin","passwordHash":`,
		"second JSON value": `{"schemaVersion":1,"username":"a","passwordHash":"x"} {}`,
		"trailing garbage":  `{"schemaVersion":1,"username":"a","passwordHash":"x"}garbage`,
		"unknown field":     `{"schemaVersion":1,"username":"a","passwordHash":"x","extra":1}`,
		"wrong schema":      `{"schemaVersion":2,"username":"a","passwordHash":"x"}`,
		"bad username":      `{"schemaVersion":1,"username":"bad user","passwordHash":"x"}`,
		"bad hash":          `{"schemaVersion":1,"username":"admin","passwordHash":"not-a-phc"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := credPath(t)
			writeRaw(t, path, body, 0o600)
			if _, err := Load(path); err == nil {
				t.Fatalf("%s: expected load failure", name)
			}
		})
	}
	// Oversized file: valid object padded past the bound.
	t.Run("oversized", func(t *testing.T) {
		path := credPath(t)
		writeRaw(t, path, string(validRaw)+strings.Repeat(" ", maxFile), 0o600)
		if _, err := Load(path); err == nil {
			t.Fatal("oversized admin.json accepted")
		}
	})
	// Insecure mode.
	t.Run("insecure mode", func(t *testing.T) {
		path := credPath(t)
		writeRaw(t, path, string(validRaw), 0o644)
		if _, err := Load(path); err == nil {
			t.Fatal("0644 admin.json accepted")
		}
	})
	// Symlink.
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		writeRaw(t, target, string(validRaw), 0o600)
		link := filepath.Join(dir, "admin.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); err == nil {
			t.Fatal("symlinked admin.json accepted")
		}
	})
	// Trailing whitespace remains valid.
	t.Run("trailing whitespace", func(t *testing.T) {
		path := credPath(t)
		writeRaw(t, path, strings.TrimRight(string(validRaw), "\n")+"\n\n  \n", 0o600)
		if _, err := Load(path); err != nil {
			t.Fatalf("trailing whitespace rejected: %v", err)
		}
	})
}
