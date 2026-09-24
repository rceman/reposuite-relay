// Package store implements Relay-owned durable persistence under
// <relay>/sessions — a small concrete filesystem store, not a framework.
//
// Layout per session (<id> is the RelaySession.ID, never the client key):
//
//	sessions/<id>/session.json     versioned metadata, atomically replaced
//	sessions/<id>/transcript.jsonl append-only durable records
//	sessions/<id>/transcript.idx   derived fixed-width offset index
//
// Crash safety: Create writes a `.create-<rand>` directory and renames it
// into place, so a canonical session directory either exists completely
// or not at all. Delete renames to a `.delete-<rand>` tombstone before
// removal. Open cleans only these Relay-owned temp/tombstone prefixes —
// any other unexpected entry fails closed.
//
// Runtime internals (PID, RuntimeID, StartedAt, credentials, handles) are
// NEVER persisted: a HarnessRuntime is ephemeral and dies with its
// daemon. The singleton relayd is the only writer; the store relies on
// that ownership instead of implementing its own locking.
package store

import (
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"path/filepath"
	"time"
)

// MetaVersion is the durable session.json schema version. Unknown
// versions fail startup — no silent migration.
const MetaVersion = 1

const (
	metaFile       = "session.json"
	transcriptFile = "transcript.jsonl"
	indexFile      = "transcript.idx"

	createPrefix = ".create-"
	deletePrefix = ".delete-"
	tmpPrefix    = ".tmp-"
)

// UncertainError means the operation's commit point may have passed but a
// post-commit durability step failed — canonical namespace state is
// ambiguous. Callers MUST fail closed: never treat the operation as
// rolled back, never silently continue serving ambiguous authority.
type UncertainError struct {
	Op  string
	Err error
}

func (e *UncertainError) Error() string {
	return e.Op + ": commit uncertain: " + e.Err.Error()
}

func (e *UncertainError) Unwrap() error { return e.Err }

// CleanupError means the operation logically committed; only post-commit
// cleanup failed. The outcome is safe to apply — leftover artifacts are
// recovered on the next Open. Callers must NOT roll back a committed
// operation on this error.
type CleanupError struct {
	Op  string
	Err error
}

func (e *CleanupError) Error() string {
	return e.Op + ": cleanup after commit: " + e.Err.Error()
}

func (e *CleanupError) Unwrap() error { return e.Err }

// Hooks overrides the store's filesystem primitives — a test seam for
// deterministic post-commit failure injection. Nil means production.
type Hooks struct {
	Rename    func(old, new string) error
	SyncDir   func(dir string) error
	RemoveAll func(path string) error
	WriteFile func(f *os.File, b []byte) (int, error)
	SyncFile  func(f *os.File) error
}

func defaultHooks() *Hooks {
	return &Hooks{
		Rename:    os.Rename,
		SyncDir:   syncDir,
		RemoveAll: os.RemoveAll,
		WriteFile: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		SyncFile:  func(f *os.File) error { return f.Sync() },
	}
}

// Sessions is the concrete per-state-root session store rooted at
// <relay>/sessions.
type Sessions struct {
	root  string
	newID func() (string, error) // temp-name entropy (session.NewSessionID)
	hooks *Hooks
}

// SetHooks overrides the store's filesystem primitives. Nil fields keep
// their production defaults. Test seam only — production never calls it.
func (s *Sessions) SetHooks(h *Hooks) {
	d := defaultHooks()
	if h != nil {
		if h.Rename != nil {
			d.Rename = h.Rename
		}
		if h.SyncDir != nil {
			d.SyncDir = h.SyncDir
		}
		if h.RemoveAll != nil {
			d.RemoveAll = h.RemoveAll
		}
		if h.WriteFile != nil {
			d.WriteFile = h.WriteFile
		}
		if h.SyncFile != nil {
			d.SyncFile = h.SyncFile
		}
	}
	s.hooks = d
}

// meta is the durable session.json schema — exactly the RelaySession
// fields that are durable. No runtime fields exist in this struct, so a
// runtime can never be persisted by accident.
type meta struct {
	Version               int       `json:"version"`
	SessionID             string    `json:"sessionId"`
	Key                   string    `json:"key"`
	Harness               string    `json:"harness"`
	Cwd                   string    `json:"cwd"`
	NativeSessionID       string    `json:"nativeSessionId,omitempty"`
	State                 string    `json:"state"`
	Model                 string    `json:"model,omitempty"`
	Mode                  string    `json:"mode,omitempty"`
	Generation            int       `json:"generation"`
	SeqHighWatermark      uint64    `json:"seqHighWatermark,omitempty"`
	TelemetrySeqWatermark uint64    `json:"telemetrySeqWatermark,omitempty"`
	TelemetryStartedEvent string    `json:"telemetryStartedEvent,omitempty"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

func toMeta(rs *session.RelaySession) meta {
	return meta{
		Version:               MetaVersion,
		SessionID:             rs.ID,
		Key:                   rs.Key,
		Harness:               rs.Harness,
		Cwd:                   rs.Cwd,
		NativeSessionID:       rs.NativeSessionID,
		State:                 rs.State,
		Model:                 rs.Model,
		Mode:                  rs.Mode,
		Generation:            rs.Generation,
		SeqHighWatermark:      rs.SeqHighWatermark,
		TelemetrySeqWatermark: rs.TelemetrySeqWatermark,
		TelemetryStartedEvent: rs.TelemetryStartedEvent,
		CreatedAt:             rs.CreatedAt.UTC(),
		UpdatedAt:             rs.UpdatedAt.UTC(),
	}
}

func (m meta) toSession() *session.RelaySession {
	return &session.RelaySession{
		ID:                    m.SessionID,
		Key:                   m.Key,
		Harness:               m.Harness,
		Cwd:                   m.Cwd,
		NativeSessionID:       m.NativeSessionID,
		State:                 m.State,
		Model:                 m.Model,
		Mode:                  m.Mode,
		Generation:            m.Generation,
		SeqHighWatermark:      m.SeqHighWatermark,
		TelemetrySeqWatermark: m.TelemetrySeqWatermark,
		TelemetryStartedEvent: m.TelemetryStartedEvent,
		CreatedAt:             m.CreatedAt,
		UpdatedAt:             m.UpdatedAt,
	}
}

// validate enforces the canonical metadata contract. dirID is the
// containing directory name ("" when validating a session being created).
func (m meta) validate(dirID string) error {
	if m.Version != MetaVersion {
		return fmt.Errorf("unsupported schema version %d", m.Version)
	}
	if !session.ValidID(m.SessionID) {
		return fmt.Errorf("invalid session id %q", m.SessionID)
	}
	if dirID != "" && m.SessionID != dirID {
		return fmt.Errorf("directory %s holds session id %s", dirID, m.SessionID)
	}
	if !session.ValidKey(m.Key) {
		return fmt.Errorf("invalid session key %q", m.Key)
	}
	if m.Harness == "" {
		return fmt.Errorf("empty harness")
	}
	if !filepath.IsAbs(m.Cwd) {
		return fmt.Errorf("cwd %q is not absolute", m.Cwd)
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("missing createdAt")
	}
	if m.Generation < 0 {
		return fmt.Errorf("negative generation %d", m.Generation)
	}
	return nil
}

// OpenSessions opens (creating if needed, 0700) the sessions store root
// and cleans stale Relay-owned create temps and delete tombstones. It
// does not validate canonical sessions — call LoadAll for that.
func OpenSessions(root string, newID func() (string, error)) (*Sessions, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create sessions root %s: %w", root, err)
	}
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("sessions root %s is not a directory", root)
	}
	if newID == nil {
		newID = session.NewSessionID
	}
	s := &Sessions{
		root:  root,
		newID: newID,
		hooks: defaultHooks(),
	}
	if err := s.cleanTemps(); err != nil {
		return nil, err
	}
	return s, nil
}

// cleanTemps removes stale `.create-*` and `.delete-*` entries left by
// crashed creates/deletes. Only Relay-owned prefixes on real directories
// are touched; anything else (files, symlinks) fails closed.
func (s *Sessions) cleanTemps() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read sessions root: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		isCreate := len(name) > len(createPrefix) && name[:len(createPrefix)] == createPrefix
		isDelete := len(name) > len(deletePrefix) && name[:len(deletePrefix)] == deletePrefix
		if !isCreate && !isDelete {
			continue
		}
		p := filepath.Join(s.root, name)
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("stat temp %s: %w", name, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("temp entry %s is not a plain directory (mode %s); refusing cleanup", name, fi.Mode())
		}
		if err := s.hooks.RemoveAll(p); err != nil {
			return fmt.Errorf("remove stale temp %s: %w", name, err)
		}
	}
	return nil
}
