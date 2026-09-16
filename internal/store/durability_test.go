package store

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSymlinkFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "real")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	// Symlink where canonical dir expected.
	outside := t.TempDir()
	id, _ := session.NewSessionID()
	link := filepath.Join(root, id)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("symlink canonical dir must fail closed")
	}
	os.Remove(link)
	// Symlink as session.json.
	id2, _ := session.NewSessionID()
	dir := filepath.Join(root, id2)
	os.Mkdir(dir, 0o700)
	target := filepath.Join(t.TempDir(), "x")
	os.WriteFile(target, []byte("{}"), 0o600)
	os.Symlink(target, filepath.Join(dir, "session.json"))
	os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "transcript.idx"), nil, 0o600)
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("symlink session.json must fail closed")
	}
}

func TestSaveAtomicReplace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "mut")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	rs.NativeSessionID = "native-123"
	rs.Model = "m1"
	rs.Generation = 2
	rs.UpdatedAt = time.Now()
	if err := s.Save(rs); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	got := loaded[0]
	if got.NativeSessionID != "native-123" || got.Model != "m1" || got.Generation != 2 {
		t.Fatalf("save roundtrip: %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(root, rs.ID, "session.json")); err != nil {
		t.Fatal(err)
	}
	// No temp leftovers.
	entries, _ := os.ReadDir(filepath.Join(root, rs.ID))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("save temp residue: %s", e.Name())
		}
	}
	// Save of absent session fails.
	gone := mkSession(t, "gone")
	if err := s.Save(gone); err == nil {
		t.Fatal("save on missing dir must fail")
	}
}

func TestPermissions(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	s := open(t, root)
	rs := mkSession(t, "perm")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	check := func(path string, want os.FileMode) {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Fatalf("%s mode %o want %o", path, got, want)
		}
	}
	check(root, 0o700)
	check(filepath.Join(root, rs.ID), 0o700)
	for _, f := range []string{"session.json", "transcript.jsonl", "transcript.idx"} {
		check(filepath.Join(root, rs.ID, f), 0o600)
	}
}

// TestNoRuntimeInMetadata: the durable schema must never persist runtime
// internals — PID, RuntimeID, StartedAt, credentials.
func TestNoRuntimeInMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "sec")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, rs.ID, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"runtimeId", "pid", "runtimeStartedAt",
		"startedAt", "token", "password", "secret", "credential"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("session.json must not contain runtime field %q", forbidden)
		}
	}
	if m["sessionId"] != rs.ID || m["version"] != float64(MetaVersion) {
		t.Fatalf("metadata: %v", m)
	}
}

// TestStoreChurn: synthetic create/reload/delete/reload over a few hundred
// sessions — bounded, no wall-clock assertions.
func TestStoreChurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	const n = 200
	keep := map[string]*session.RelaySession{}
	for i := 0; i < n; i++ {
		rs := mkSession(t, "s"+string(rune('a'+i%26))+string(rune('a'+i/26))+string(rune('0'+i%10))+string(rune('0'+i/10%10)))
		rs.Key = keyN(i)
		if err := s.Create(rs); err != nil {
			t.Fatal(err)
		}
		keep[rs.ID] = rs
	}
	// Delete every third session.
	i := 0
	for id := range keep {
		if i%3 == 0 {
			if err := s.Delete(id); err != nil {
				t.Fatal(err)
			}
			delete(keep, id)
		}
		i++
	}
	s2 := open(t, root)
	loaded, err := s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(keep) {
		t.Fatalf("loaded %d want %d", len(loaded), len(keep))
	}
	for _, rs := range loaded {
		want, ok := keep[rs.ID]
		if !ok || want.Key != rs.Key {
			t.Fatalf("unexpected session %s key=%s", rs.ID, rs.Key)
		}
	}
}
