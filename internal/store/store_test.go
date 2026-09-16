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

func keyN(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	a, b, c, d := alpha[i%26], alpha[(i/26)%26], alpha[(i/676)%26], alpha[(i/17576)%26]
	return string([]byte{a, b, c, d})
}
