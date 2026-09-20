package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	want := Config{
		SchemaVersion: SchemaVersion,
		ListenHost:    ListenHost,
		ListenPort:    17432,
	}
	if err := Commit(path, want); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o, want 0600", fi.Mode().Perm())
	}
	if got.Address() != "127.0.0.1:17432" {
		t.Fatalf("address %q", got.Address())
	}
}

func TestLoadMissingIsNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "relay.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadMalformedFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"not-json":       "garbage",
		"truncated":      `{"schemaVersion": 1, "listen`,
		"unknown-field":  `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":1,"extra":true}`,
		"bad-schema":     `{"schemaVersion":2,"listenHost":"127.0.0.1","listenPort":1}`,
		"missing-schema": `{"listenHost":"127.0.0.1","listenPort":1}`,
		"lan-host":       `{"schemaVersion":1,"listenHost":"0.0.0.0","listenPort":1}`,
		"hostname":       `{"schemaVersion":1,"listenHost":"localhost","listenPort":1}`,
		"port-zero":      `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":0}`,
		"port-negative":  `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":-1}`,
		"port-overflow":  `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":65536}`,
		"port-string":    `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":"1"}`,
	} {
		path := filepath.Join(t.TempDir(), "relay.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%s: malformed config must fail closed", name)
		}
		// The file is never silently rewritten.
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatalf("%s: malformed config was rewritten", name)
		}
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "relay.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink config must fail closed")
	}
}

func TestCommitRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	for _, c := range []Config{
		{SchemaVersion: 0, ListenHost: ListenHost, ListenPort: 1},
		{SchemaVersion: SchemaVersion, ListenHost: "0.0.0.0", ListenPort: 1},
		{SchemaVersion: SchemaVersion, ListenHost: ListenHost, ListenPort: 0},
		{SchemaVersion: SchemaVersion, ListenHost: ListenHost, ListenPort: 70000},
	} {
		if err := Commit(path, c); err == nil {
			t.Fatalf("commit of %+v must fail", c)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid commit left a config file")
	}
}

func TestCommitAtomicNoPartialFile(t *testing.T) {
	// A failed commit (unwritable directory) must leave no torn file.
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.json")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	err := Commit(path, Config{
		SchemaVersion: 1,
		ListenHost:    ListenHost,
		ListenPort:    1,
	})
	if err == nil {
		t.Skip("environment permits writes to 0500 dir")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".relay.json.tmp-") {
			t.Fatalf("torn temp file left behind: %s", e.Name())
		}
	}
}
