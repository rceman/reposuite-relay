package daemon

// Daemon-level proof for store commit-point semantics: after a create
// commit point the daemon must never continue serving ambiguous
// authority, and after a committed delete it must never resurrect the
// session because cleanup failed.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/store"
)

var errInjected = errors.New("injected fault")

// TestCreateUncertainHaltsDaemon: create rename committed + root sync
// failure → daemon returns INTERNAL and halts; the canonical session
// exists on disk and the next daemon start owns it (no ghost, no
// duplicate window).
func TestCreateUncertainHaltsDaemon(t *testing.T) {
	_, p, c, served := startInProcess(t, func(o *Options) {
		o.StoreHooks = &store.Hooks{
			SyncDir: func(dir string) error {
				if filepath.Base(dir) == "sessions" {
					return errInjected // post-rename root sync only
				}
				return syncDirReal(dir)
			},
		}
	})
	_, err := serve(t, c, "ghost")
	wantAPIErr(t, err, api.ErrInternal)
	// The daemon must halt rather than keep serving with ambiguous state.
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon must shut down after an uncertain store commit")
	}
	// Canonical truth: the session directory exists.
	sessionsDir := p.SessionsDir()
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if len(e.Name()) == 32 { // canonical session id
			found++
		}
	}
	if found != 1 {
		t.Fatalf("canonical session dirs = %d, want 1", found)
	}
	// A fresh daemon owns the session — no duplicate create possible.
	ss, err := store.OpenSessions(sessionsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ss.LoadAll()
	if err != nil || len(loaded) != 1 || loaded[0].Key != "ghost" {
		t.Fatalf("next startup must own the committed session: %v %v", loaded, err)
	}
}

// TestDeleteCleanupFailureStillDeletes: delete commit + tombstone cleanup
// failure → stop reports success, the session is gone from disk and the
// registry, and the tombstone is recovered at next start.
func TestDeleteCleanupFailureStillDeletes(t *testing.T) {
	_, p, c, _ := startInProcess(t, func(o *Options) {
		o.StoreHooks = &store.Hooks{
			RemoveAll: func(path string) error {
				if filepath.Base(filepath.Dir(path)) == "sessions" &&
					len(filepath.Base(path)) > 8 && filepath.Base(path)[:8] == ".delete-" {
					return errInjected // tombstone removal only
				}
				return os.RemoveAll(path)
			},
		}
	})
	if _, err := serve(t, c, "victim"); err != nil {
		t.Fatalf("serve: %v", err)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.StopSession(ctx, "victim"); err != nil {
		t.Fatalf("committed delete must succeed despite cleanup failure: %v", err)
	}
	// Registry: gone.
	if _, err := c.Status(ctx, "victim"); err == nil {
		t.Fatal("session must be gone")
	}
	// Disk: canonical dir gone (tombstone may remain for recovery).
	entries, err := os.ReadDir(p.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) == 32 {
			t.Fatalf("canonical session dir must be gone: %s", e.Name())
		}
	}
}

// syncDirReal mirrors the store's syncDir for hook composition.
func syncDirReal(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
