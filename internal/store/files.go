package store

import (
	"fmt"
	"os"

	"encoding/json"
	"path/filepath"
)

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
func writeMetaFile(h *Hooks, dir string, m meta) error {
	return writeFileSync(h, filepath.Join(dir, metaFile), mustMarshal(m), 0o600)
}

func mustMarshal(m meta) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic(err) // meta is statically marshalable
	}
	return append(b, '\n')
}

// writeEmptyFile creates an empty 0600 synced file.
func writeEmptyFile(h *Hooks, path string) error {
	return writeFileSync(h, path, nil, 0o600)
}

// writeFileSync writes path with mode, fsyncs, and closes.
func writeFileSync(h *Hooks, path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := h.WriteFile(f, data); err != nil {
		f.Close()
		return err
	}
	if err := h.SyncFile(f); err != nil {
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
