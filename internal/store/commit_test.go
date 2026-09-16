package store

// Commit-point semantics: deterministic post-commit failure injection
// proving the store never reports the opposite of canonical truth.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/session"
)

var errInject = errors.New("injected fault")

// hooked opens a store with the given hook overrides.
func hooked(t *testing.T, root string, h *Hooks) *Sessions {
	t.Helper()
	s, err := OpenSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := defaultHooks()
	if h.Rename != nil {
		base.Rename = h.Rename
	}
	if h.SyncDir != nil {
		base.SyncDir = h.SyncDir
	}
	if h.RemoveAll != nil {
		base.RemoveAll = h.RemoveAll
	}
	if h.WriteFile != nil {
		base.WriteFile = h.WriteFile
	}
	if h.SyncFile != nil {
		base.SyncFile = h.SyncFile
	}
	s.SetHooks(base)
	return s
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// --- Create -----------------------------------------------------------

func TestCreatePreCommitFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	var s *Sessions
	s = hooked(t, root, &Hooks{
		Rename: func(a, b string) error { return errInject },
	})
	rs := mkSession(t, "pre")
	err := s.Create(rs)
	if err == nil || errors.As(err, new(*UncertainError)) {
		t.Fatalf("pre-commit rename failure must be a plain error: %v", err)
	}
	if exists(t, filepath.Join(root, rs.ID)) {
		t.Fatal("canonical dir must not exist after pre-commit failure")
	}
	// Temp residue is cleaned; next Open is healthy.
	if _, err := s.LoadAll(); err != nil || len(mustLoad(t, s)) != 0 {
		t.Fatal("store must be clean")
	}
	// A following create with healthy hooks works.
	s.SetHooks(nil)
	rs2 := mkSession(t, "post")
	if err := s.Create(rs2); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, s *Sessions) []*session.RelaySession {
	t.Helper()
	l, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCreatePostRenameSyncFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	realSync := syncDir
	s := hooked(t, root, &Hooks{
		SyncDir: func(dir string) error {
			if dir == root {
				return errInject // only the post-rename root sync
			}
			return realSync(dir)
		},
	})
	rs := mkSession(t, "ghost")
	err := s.Create(rs)
	var ue *UncertainError
	if !errors.As(err, &ue) {
		t.Fatalf("want UncertainError, got %v", err)
	}
	// Canonical truth: the rename committed — dir exists. The store must
	// never report this as "session absent".
	if !exists(t, filepath.Join(root, rs.ID)) {
		t.Fatal("canonical dir must exist — commit point passed")
	}
	// Next open loads it: no unowned ghost.
	loaded, err := s.LoadAll()
	if err != nil || len(loaded) != 1 || loaded[0].ID != rs.ID {
		t.Fatalf("canonical session must load: %v %v", loaded, err)
	}
}

// --- Save -------------------------------------------------------------

func TestSavePostRenameSyncFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "sv")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	realSync := syncDir
	dir := filepath.Join(root, rs.ID)
	s.SetHooks(&Hooks{
		Rename: os.Rename,
		SyncDir: func(d string) error {
			if d == dir {
				return errInject
			}
			return realSync(d)
		},
		RemoveAll: os.RemoveAll,
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
	})
	rs.Model = "m2"
	err := s.Save(rs)
	var ue *UncertainError
	if !errors.As(err, &ue) {
		t.Fatalf("want UncertainError, got %v", err)
	}
	// Namespace truth: NEW metadata is in place — never assume old.
	loaded := mustLoad(t, s)
	if loaded[0].Model != "m2" {
		t.Fatalf("new metadata must be visible: %+v", loaded[0])
	}
}

func TestSavePreCommitFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "sv2")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	s.SetHooks(&Hooks{
		Rename:    func(a, b string) error { return errInject },
		SyncDir:   syncDir,
		RemoveAll: os.RemoveAll,
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
	})
	rs.Model = "m2"
	err := s.Save(rs)
	if err == nil || errors.As(err, new(*UncertainError)) {
		t.Fatalf("pre-commit save failure must be plain: %v", err)
	}
	if got := mustLoad(t, s)[0].Model; got != "" {
		t.Fatalf("old metadata must remain: %q", got)
	}
}

// --- Delete -----------------------------------------------------------

func TestDeletePreCommitFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "del")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	s.SetHooks(&Hooks{
		Rename:    func(a, b string) error { return errInject },
		SyncDir:   syncDir,
		RemoveAll: os.RemoveAll,
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
	})
	err := s.Delete(rs.ID)
	if err == nil || errors.As(err, new(*UncertainError)) || errors.As(err, new(*CleanupError)) {
		t.Fatalf("pre-commit delete failure must be plain: %v", err)
	}
	if !exists(t, filepath.Join(root, rs.ID)) {
		t.Fatal("canonical dir must still exist — delete did not commit")
	}
}

func TestDeleteCommittedSyncFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "del2")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	realSync := syncDir
	s.SetHooks(&Hooks{
		Rename: os.Rename,
		SyncDir: func(d string) error {
			if d == root {
				return errInject
			}
			return realSync(d)
		},
		RemoveAll: os.RemoveAll,
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
	})
	err := s.Delete(rs.ID)
	var ue *UncertainError
	if !errors.As(err, &ue) {
		t.Fatalf("want UncertainError, got %v", err)
	}
	if exists(t, filepath.Join(root, rs.ID)) {
		t.Fatal("canonical dir must be gone — delete committed")
	}
	// Tombstone may remain; next Open recovers it.
	s2 := open(t, root)
	loaded, err := s2.LoadAll()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("session must stay deleted after recovery: %v %v", loaded, err)
	}
}

func TestDeleteCommittedCleanupFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	s := open(t, root)
	rs := mkSession(t, "del3")
	if err := s.Create(rs); err != nil {
		t.Fatal(err)
	}
	realSync := syncDir
	calls := 0
	s.SetHooks(&Hooks{
		Rename:    os.Rename,
		RemoveAll: func(p string) error { return errInject },
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
		SyncDir: func(d string) error {
			calls++
			if calls > 1 { // first sync commits; fail the cleanup sync too
				return errInject
			}
			return realSync(d)
		},
	})
	err := s.Delete(rs.ID)
	var ce *CleanupError
	if !errors.As(err, &ce) {
		t.Fatalf("want CleanupError, got %v", err)
	}
	if exists(t, filepath.Join(root, rs.ID)) {
		t.Fatal("canonical dir must be gone — deletion committed")
	}
	// Tombstone remains but is recognized and cleaned at next Open.
	s2 := open(t, root)
	loaded, err := s2.LoadAll()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("session must stay deleted: %v %v", loaded, err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".delete-") {
			t.Fatalf("tombstone not recovered: %s", e.Name())
		}
	}
}

// --- Transcript append ------------------------------------------------

func TestAppendIdxWriteFailureRepairs(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 3)
	// Fail only writes to the live index file; rebuild writes to a temp.
	tr.hooks.WriteFile = func(f *os.File, b []byte) (int, error) {
		if f == tr.idx {
			return 0, errInject
		}
		return f.Write(b)
	}
	if err := tr.Append(rec(4, "x")); err != nil {
		t.Fatalf("append must recover via rebuild: %v", err)
	}
	recs, through, err := tr.Tail(10)
	if err != nil || len(recs) != 4 || through != 4 {
		t.Fatalf("state must be consistent: %v through=%d %v", seqs(recs), through, err)
	}
	// Follow-up appends stay monotonic — no seq reuse.
	tr.hooks.WriteFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	if err := tr.Append(rec(5, "y")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(rec(4, "reuse")); err == nil {
		t.Fatal("seq reuse must be rejected")
	}
}

func TestAppendIdxSyncFailureRepairs(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 2)
	tr.hooks.SyncFile = func(f *os.File) error {
		if f == tr.idx {
			return errInject
		}
		return f.Sync()
	}
	if err := tr.Append(rec(3, "x")); err != nil {
		t.Fatalf("append must recover via rebuild: %v", err)
	}
	if tr.Len() != 3 {
		t.Fatalf("len=%d", tr.Len())
	}
}

func TestAppendJSONLSyncFailurePoisons(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 2)
	tr.hooks.SyncFile = func(f *os.File) error {
		if f == tr.jsonl {
			return errInject
		}
		return f.Sync()
	}
	err := tr.Append(rec(3, "x"))
	var ue *UncertainError
	if !errors.As(err, &ue) {
		t.Fatalf("want UncertainError, got %v", err)
	}
	// Poisoned: every subsequent op fails deterministically.
	if err := tr.Append(rec(4, "next")); err == nil {
		t.Fatal("poisoned transcript must reject appends")
	}
	if _, _, err := tr.Tail(1); err == nil {
		t.Fatal("poisoned transcript must reject tail")
	}
}

func TestAppendJSONLWriteFailurePoisons(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	tr.hooks.WriteFile = func(f *os.File, b []byte) (int, error) {
		if f == tr.jsonl {
			return 0, errInject
		}
		return f.Write(b)
	}
	if err := tr.Append(rec(1, "x")); err == nil {
		t.Fatal("jsonl write failure must error")
	}
	if err := tr.Append(rec(2, "next")); err == nil {
		t.Fatal("poisoned transcript must reject appends")
	}
}

func TestAppendIdxFailureAndRepairFailurePoisons(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 2)
	// Index write fails AND the rebuild's rename fails → poison.
	tr.hooks.WriteFile = func(f *os.File, b []byte) (int, error) {
		if f == tr.idx {
			return 0, errInject
		}
		return f.Write(b)
	}
	tr.hooks.Rename = func(a, b string) error { return errInject }
	err := tr.Append(rec(3, "x"))
	var ue *UncertainError
	if !errors.As(err, &ue) {
		t.Fatalf("want UncertainError, got %v", err)
	}
	if err := tr.Append(rec(4, "next")); err == nil {
		t.Fatal("poisoned transcript must reject appends")
	}
	if _, _, err := tr.Tail(1); err == nil {
		t.Fatal("poisoned transcript must reject tail")
	}
}

func TestRebuildRenameFailurePoisons(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 3)
	tr.Close()
	// Force a rebuild: remove the index, then fail the rebuild rename.
	os.Remove(filepath.Join(dir, "transcript.idx"))
	tr2, err := OpenTranscript(dir) // healthy rebuild first
	if err != nil {
		t.Fatal(err)
	}
	tr2.Close()
	os.Remove(filepath.Join(dir, "transcript.idx"))
	// Open with a poisoned rename hook.
	jp := filepath.Join(dir, "transcript.jsonl")
	jf, _ := os.OpenFile(jp, os.O_RDWR|os.O_APPEND, 0o600)
	idx, _ := os.OpenFile(filepath.Join(dir, "transcript.idx"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	tr = &Transcript{dir: dir, jsonl: jf, idx: idx, hooks: defaultHooks()}
	tr.hooks.Rename = func(a, b string) error { return errInject }
	if err := tr.openCheck(); err == nil {
		t.Fatal("rebuild with failing rename must error")
	}
}
