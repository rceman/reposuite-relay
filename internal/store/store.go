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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
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

// Sessions is the concrete per-state-root session store rooted at
// <relay>/sessions.
type Sessions struct {
	root  string
	newID func() (string, error) // temp-name entropy (session.NewSessionID)
}

// meta is the durable session.json schema — exactly the RelaySession
// fields that are durable. No runtime fields exist in this struct, so a
// runtime can never be persisted by accident.
type meta struct {
	Version         int       `json:"version"`
	SessionID       string    `json:"sessionId"`
	Key             string    `json:"key"`
	Harness         string    `json:"harness"`
	Cwd             string    `json:"cwd"`
	NativeSessionID string    `json:"nativeSessionId,omitempty"`
	State           string    `json:"state"`
	Model           string    `json:"model,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	Generation      int       `json:"generation"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func toMeta(rs *session.RelaySession) meta {
	return meta{
		Version:         MetaVersion,
		SessionID:       rs.ID,
		Key:             rs.Key,
		Harness:         rs.Harness,
		Cwd:             rs.Cwd,
		NativeSessionID: rs.NativeSessionID,
		State:           rs.State,
		Model:           rs.Model,
		Mode:            rs.Mode,
		Generation:      rs.Generation,
		CreatedAt:       rs.CreatedAt.UTC(),
		UpdatedAt:       rs.UpdatedAt.UTC(),
	}
}

func (m meta) toSession() *session.RelaySession {
	return &session.RelaySession{
		ID:              m.SessionID,
		Key:             m.Key,
		Harness:         m.Harness,
		Cwd:             m.Cwd,
		NativeSessionID: m.NativeSessionID,
		State:           m.State,
		Model:           m.Model,
		Mode:            m.Mode,
		Generation:      m.Generation,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
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
	s := &Sessions{root: root, newID: newID}
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
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove stale temp %s: %w", name, err)
		}
	}
	return nil
}

// LoadAll reads and validates every canonical session directory.
// Malformed metadata, unexpected entries, duplicates, or symlinks fail —
// a corrupt durable store is a daemon startup error, never skipped.
func (s *Sessions) LoadAll() ([]*session.RelaySession, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read sessions root: %w", err)
	}
	var out []*session.RelaySession
	keys := map[string]string{} // key -> id
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(s.root, name)
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", name, err)
		}
		if !session.ValidID(name) {
			return nil, fmt.Errorf("unrecognized entry %q in sessions root", name)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("session %s is not a plain directory (mode %s)", name, fi.Mode())
		}
		rs, err := s.load(name)
		if err != nil {
			return nil, err
		}
		if prev, ok := keys[rs.Key]; ok {
			return nil, fmt.Errorf("duplicate session key %q (ids %s, %s)", rs.Key, prev, rs.ID)
		}
		keys[rs.Key] = rs.ID
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// load reads and validates one canonical session directory.
func (s *Sessions) load(id string) (*session.RelaySession, error) {
	dir := filepath.Join(s.root, id)
	raw, err := readRegular(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("load session %s: decode %s: %w", id, metaFile, err)
	}
	if err := m.validate(id); err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	// Structural completeness: every canonical dir has a transcript pair.
	for _, f := range []string{transcriptFile, indexFile} {
		fi, err := os.Lstat(filepath.Join(dir, f))
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("load session %s: %s missing or not a regular file", id, f)
		}
	}
	return m.toSession(), nil
}

// readRegular reads a file that must exist as a plain regular file
// (not a symlink — canonical store objects are never symlinks).
func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, fi.Mode())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Create atomically persists a new RelaySession: builds a complete
// `.create-<rand>` directory (metadata + empty transcript pair), then
// renames it into the canonical position. A crash leaves at most a stale
// temp cleaned by the next Open — never a half-valid canonical session.
func (s *Sessions) Create(rs *session.RelaySession) error {
	if err := toMeta(rs).validate(""); err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	canonical := filepath.Join(s.root, rs.ID)
	if _, err := os.Lstat(canonical); err == nil {
		return fmt.Errorf("create session %s: already exists", rs.ID)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}

	tmp, err := s.newTemp(createPrefix)
	if err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := writeMetaFile(tmp, toMeta(rs)); err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	for _, f := range []string{transcriptFile, indexFile} {
		if err := writeEmptyFile(filepath.Join(tmp, f)); err != nil {
			return fmt.Errorf("create session %s: %w", rs.ID, err)
		}
	}
	if err := syncDir(tmp); err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	if err := os.Rename(tmp, canonical); err != nil {
		return fmt.Errorf("create session %s: rename into place: %w", rs.ID, err)
	}
	if err := syncDir(s.root); err != nil {
		return fmt.Errorf("create session %s: sync root: %w", rs.ID, err)
	}
	ok = true
	return nil
}

// Save atomically replaces session.json (temp write + fsync + rename) for
// an existing canonical session — the primitive later used for native
// session IDs, generation increments, and model/mode/state changes.
func (s *Sessions) Save(rs *session.RelaySession) error {
	if err := toMeta(rs).validate(""); err != nil {
		return fmt.Errorf("save session %s: %w", rs.ID, err)
	}
	dir := filepath.Join(s.root, rs.ID)
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("save session %s: canonical directory absent", rs.ID)
	}
	m := toMeta(rs)
	tmp, err := s.newID()
	if err != nil {
		return fmt.Errorf("save session %s: %w", rs.ID, err)
	}
	tmpPath := filepath.Join(dir, tmpPrefix+tmp)
	if err := writeFileSync(tmpPath, mustMarshal(m), 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("save session %s: %w", rs.ID, err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, metaFile)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("save session %s: rename: %w", rs.ID, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("save session %s: %w", rs.ID, err)
	}
	return nil
}

// Delete durably removes a session: rename to a `.delete-*` tombstone
// (the logical delete commit point) then remove contents. A crash after
// the rename leaves a tombstone the next Open cleans — never a
// half-deleted canonical session.
func (s *Sessions) Delete(id string) error {
	if !session.ValidID(id) {
		return fmt.Errorf("delete session %q: invalid id", id)
	}
	canonical := filepath.Join(s.root, id)
	fi, err := os.Lstat(canonical)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("delete session %s: canonical directory absent", id)
	}
	rnd, err := s.newID()
	if err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	tomb := filepath.Join(s.root, deletePrefix+id+"-"+rnd[:8])
	if err := os.Rename(canonical, tomb); err != nil {
		return fmt.Errorf("delete session %s: tombstone rename: %w", id, err)
	}
	// The session is logically deleted from this point on.
	if err := os.RemoveAll(tomb); err != nil {
		return fmt.Errorf("delete session %s: remove tombstone: %w", id, err)
	}
	if err := syncDir(s.root); err != nil {
		return fmt.Errorf("delete session %s: sync root: %w", id, err)
	}
	return nil
}

// Transcript opens the durable transcript of an existing canonical
// session for appending/tail reads.
func (s *Sessions) Transcript(id string) (*Transcript, error) {
	if !session.ValidID(id) {
		return nil, fmt.Errorf("transcript %q: invalid id", id)
	}
	dir := filepath.Join(s.root, id)
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("transcript %s: canonical directory absent", id)
	}
	return OpenTranscript(dir)
}

// newTemp makes a fresh `.create-<rand>` directory.
func (s *Sessions) newTemp(prefix string) (string, error) {
	for i := 0; i < 8; i++ {
		rnd, err := s.newID()
		if err != nil {
			return "", err
		}
		p := filepath.Join(s.root, prefix+rnd[:16])
		if err := os.Mkdir(p, 0o700); err == nil {
			return p, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate temp directory under %s", s.root)
}

// writeMetaFile writes session.json (0600, synced) inside dir.
func writeMetaFile(dir string, m meta) error {
	return writeFileSync(filepath.Join(dir, metaFile), mustMarshal(m), 0o600)
}

func mustMarshal(m meta) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic(err) // meta is statically marshalable
	}
	return append(b, '\n')
}

// writeEmptyFile creates an empty 0600 synced file.
func writeEmptyFile(path string) error {
	return writeFileSync(path, nil, 0o600)
}

// writeFileSync writes path with mode, fsyncs, and closes.
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs a directory so entry changes are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
