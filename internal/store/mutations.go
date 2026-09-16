package store

import (
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"path/filepath"
)

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
			_ = s.hooks.RemoveAll(tmp)
		}
	}()

	if err := writeMetaFile(s.hooks, tmp, toMeta(rs)); err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	for _, f := range []string{transcriptFile, indexFile} {
		if err := writeEmptyFile(s.hooks, filepath.Join(tmp, f)); err != nil {
			return fmt.Errorf("create session %s: %w", rs.ID, err)
		}
	}
	if err := s.hooks.SyncDir(tmp); err != nil {
		return fmt.Errorf("create session %s: %w", rs.ID, err)
	}
	// COMMIT POINT: the rename publishes the canonical session directory.
	// Everything before this failure is rolled back cleanly; after it the
	// session exists in the namespace and must not be reported absent.
	if err := s.hooks.Rename(tmp, canonical); err != nil {
		return fmt.Errorf("create session %s: rename into place: %w", rs.ID, err)
	}
	ok = true // committed — the deferred cleanup must not remove it
	if err := s.hooks.SyncDir(s.root); err != nil {
		return &UncertainError{
			Op:  "create session " + rs.ID,
			Err: fmt.Errorf("canonical directory committed but root sync failed: %w", err),
		}
	}
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
	if err := writeFileSync(s.hooks, tmpPath, mustMarshal(m), 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("save session %s: %w", rs.ID, err)
	}
	// COMMIT POINT: rename publishes the new metadata. After it succeeds
	// the namespace holds the NEW record — callers must not assume the old
	// one is authoritative.
	if err := s.hooks.Rename(tmpPath, filepath.Join(dir, metaFile)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("save session %s: rename: %w", rs.ID, err)
	}
	if err := s.hooks.SyncDir(dir); err != nil {
		return &UncertainError{
			Op:  "save session " + rs.ID,
			Err: fmt.Errorf("metadata committed but dir sync failed: %w", err),
		}
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
	// COMMIT POINT: once the canonical name is gone the session is
	// logically deleted — tombstone removal afterwards is cleanup only,
	// never a reason to resurrect the session.
	if err := s.hooks.Rename(canonical, tomb); err != nil {
		return fmt.Errorf("delete session %s: tombstone rename: %w", id, err)
	}
	if err := s.hooks.SyncDir(s.root); err != nil {
		return &UncertainError{
			Op:  "delete session " + id,
			Err: fmt.Errorf("deletion committed but root sync failed: %w", err),
		}
	}
	if err := s.hooks.RemoveAll(tomb); err != nil {
		return &CleanupError{
			Op:  "delete session " + id,
			Err: fmt.Errorf("remove tombstone: %w", err),
		}
	}
	if err := s.hooks.SyncDir(s.root); err != nil {
		return &CleanupError{
			Op:  "delete session " + id,
			Err: fmt.Errorf("sync root after tombstone removal: %w", err),
		}
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
	tr, err := OpenTranscript(dir)
	if err != nil {
		return nil, err
	}
	tr.hooks = s.hooks
	return tr, nil
}
