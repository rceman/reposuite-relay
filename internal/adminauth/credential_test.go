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

func TestPHCParserBounds(t *testing.T) {
	valid, err := Hash("a-valid-test-password", TestParams)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"wrong algorithm":   strings.Replace(valid, "argon2id", "argon2i", 1),
		"wrong version":     strings.Replace(valid, "v=19", "v=16", 1),
		"missing field":     valid[:strings.LastIndex(valid, "$")],
		"zero memory":       "$argon2id$v=19$m=0,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"absurd memory":     "$argon2id$v=19$m=99999999,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"absurd iterations": "$argon2id$v=19$m=65536,t=999,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"duplicate field":   "$argon2id$v=19$m=65536,m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"short salt":        "$argon2id$v=19$m=8192,t=1,p=1$AAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"short key":         "$argon2id$v=19$m=8192,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"bad base64":        "$argon2id$v=19$m=8192,t=1,p=1$AAA!!!AA!!!AAA!!!A$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePHC(phc); err == nil {
				t.Fatalf("%s: parser accepted %q", name, phc)
			}
			if Verify("whatever", phc) {
				t.Fatalf("%s: Verify accepted %q", name, phc)
			}
		})
	}
}

func TestPasswordPolicyAndVerify(t *testing.T) {
	for name, pw := range map[string]string{
		"too short": "short",
		"NUL":       "validlengthpass\x00word",
		"bad UTF-8": string([]byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 0xff, 0xfe, 'x', 'y'}),
		"too long":  strings.Repeat("x", 257),
	} {
		if err := ValidatePassword(pw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := ValidatePassword("exactly-12ch"); err != nil {
		t.Fatalf("12-byte password rejected: %v", err)
	}
	hash, err := Hash("correct horse battery", TestParams)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify("correct horse battery", hash) {
		t.Fatal("Verify rejected the correct password")
	}
	if Verify("wrong password 123", hash) {
		t.Fatal("Verify accepted a wrong password")
	}
	if err := ProductionParams.Validate(); err != nil {
		t.Fatalf("production params invalid: %v", err)
	}
}
