package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
)

func mkSession(t *testing.T, key string) *session.RelaySession {
	t.Helper()
	id, err := session.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return &session.RelaySession{
		ID: id, Key: key, Harness: session.HarnessFixture, Cwd: "/work",
		State: session.StateIdle, Generation: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

func open(t *testing.T, root string) *Sessions {
	t.Helper()
	s, err := OpenSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateLoadRoundtrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "alpha")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	// Reopen: canonical dir must exist completely.
	s2 := open(t, root)
	loaded, err := s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d sessions", len(loaded))
	}
	got := loaded[0]
	if got.ID != rs.ID || got.Key != "alpha" || got.Harness != session.HarnessFixture ||
		got.Cwd != "/work" || got.State != session.StateIdle || got.Generation != 1 ||
		!got.CreatedAt.Equal(rs.CreatedAt.UTC()) || got.NativeSessionID != "" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// Structural completeness: transcript pair exists.
	dir := filepath.Join(root, rs.ID)
	for _, f := range []string{"session.json", "transcript.jsonl", "transcript.idx"} {
		if fi, err := os.Lstat(filepath.Join(dir, f)); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s missing or not regular: %v", f, err)
		}
	}
	// No temp residue.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".create-") || strings.HasPrefix(e.Name(), ".delete-") {
			t.Fatalf("temp residue %s", e.Name())
		}
	}
}

func TestCreateDuplicateIDRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "a")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	rs2 := mkSession(t, "b")
	rs2.ID = rs.ID
	if err := s.Create(rs2); err == nil {
		t.Fatal("duplicate session id must fail")
	}
}

func TestLoadValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)

	corrupt := func(t *testing.T, name string, mutate func(m *meta), dirName string) {
		m := toMeta(mkSession(t, "k"))
		m.SessionID = dirName // keep dir/metadata consistent unless testing that check
		if mutate != nil {
			mutate(&m)
		}
		dir := filepath.Join(root, dirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, "session.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600)
		os.WriteFile(filepath.Join(dir, "transcript.idx"), nil, 0o600)
		if _, err := s.LoadAll(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("want error containing %q, got %v", name, err)
		}
		os.RemoveAll(dir)
	}

	validID, _ := session.NewSessionID()
	corrupt(t, "version", func(m *meta) { m.Version = 99 }, validID)
	id2, _ := session.NewSessionID()
	corrupt(t, "holds session id", func(m *meta) {
		other, _ := session.NewSessionID()
		m.SessionID = other // dir name != metadata id
	}, id2)
	id3, _ := session.NewSessionID()
	corrupt(t, "invalid session key", func(m *meta) { m.Key = "bad key!" }, id3)
	id4, _ := session.NewSessionID()
	corrupt(t, "empty harness", func(m *meta) { m.Harness = "" }, id4)
	id5, _ := session.NewSessionID()
	corrupt(t, "not absolute", func(m *meta) { m.Cwd = "rel/dir" }, id5)
	id6, _ := session.NewSessionID()
	corrupt(t, "invalid session id", func(m *meta) { m.SessionID = "zzz" }, id6)

	// Malformed JSON.
	id7, _ := session.NewSessionID()
	dir := filepath.Join(root, id7)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "session.json"), []byte("{broken"), 0o600)
	os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "transcript.idx"), nil, 0o600)
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("malformed metadata must fail")
	}
	os.RemoveAll(dir)

	// Unrecognized entry fails closed.
	if err := os.WriteFile(filepath.Join(root, "junk.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("unrecognized entry must fail closed")
	}
	os.Remove(filepath.Join(root, "junk.txt"))
}

func TestDuplicateKeyLoad(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	a, b := mkSession(t, "dup"), mkSession(t, "dup")
	for _, rs := range []*session.RelaySession{a, b} {
		if err := s.Create(rs); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.LoadAll(); err == nil {
		t.Fatal("duplicate persisted key must fail LoadAll")
	}
}

func TestStaleTempRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "ok")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	// Plant stale create temps and a delete tombstone.
	for _, name := range []string{".create-abc123", ".create-def456", ".delete-x-y"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, name, "session.json"), []byte("{}"), 0o600)
	}
	s2 := open(t, root)
	loaded, err := s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != rs.ID {
		t.Fatalf("temps must be cleaned, canonical kept: %+v", loaded)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("stale temp survived: %s", e.Name())
		}
	}
	// A non-directory temp (symlink/file) fails closed — never removed.
	bad := filepath.Join(root, ".create-suspicious")
	os.WriteFile(bad, []byte("x"), 0o600)
	if _, err := OpenSessions(root, nil); err == nil {
		t.Fatal("non-directory temp entry must fail closed")
	}
	os.Remove(bad)
}

func TestDeleteSemantics(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "gone")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(rs.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, rs.ID)); !os.IsNotExist(err) {
		t.Fatal("canonical dir must be gone")
	}
	loaded, err := s.LoadAll()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("deleted session must not load: %v %v", loaded, err)
	}
	if err := s.Delete(rs.ID); err == nil {
		t.Fatal("delete of absent session must fail")
	}
	if err := s.Delete("../escape"); err == nil {
		t.Fatal("delete of non-id must fail")
	}
	// Tombstone left by a "crashed" delete is cleaned on open.
	rs2 := mkSession(t, "gone2")
	if err := s.Create(rs2); err != nil {
		t.Fatal(err)
	}
	tomb := filepath.Join(root, ".delete-"+rs2.ID+"-zz")
	if err := os.Rename(filepath.Join(root, rs2.ID), tomb); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, root)
	loaded, err = s2.LoadAll()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("tombstone must be cleaned: %v %v", loaded, err)
	}
}

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

func keyN(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	a, b, c, d := alpha[i%26], alpha[(i/26)%26], alpha[(i/676)%26], alpha[(i/17576)%26]
	return string([]byte{a, b, c, d})
}
