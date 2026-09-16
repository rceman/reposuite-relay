package store

import (
	"errors"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"path/filepath"
	"testing"
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
