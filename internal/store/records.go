package store

import (
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/session"
	"io"
	"os"
	"path/filepath"
	"sort"
)

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
