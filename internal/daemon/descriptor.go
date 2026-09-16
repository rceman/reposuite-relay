// Runtime descriptor: the user-private daemon.json publishing the current
// daemon generation's loopback endpoint, instance identity, and local
// bearer token (ADR-006). Publication is daemon readiness; removal only
// ever happens for the owning instance.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// newInstanceID returns a fresh 128-bit daemon generation identity.
func newInstanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// newToken returns a fresh 256-bit local bearer token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// writeDescriptor publishes the descriptor atomically (same-dir temp,
// fsync, rename, dir sync) with 0600 permissions.
func writeDescriptor(p paths.Paths, d api.Descriptor) error {
	dir := p.RunDir()
	tmp, err := os.CreateTemp(dir, ".daemon.json.tmp-*")
	if err != nil {
		return fmt.Errorf("descriptor temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("descriptor: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, p.DaemonDescriptor()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("descriptor rename: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("descriptor dir sync: %w", err)
	}
	return nil
}

// readDescriptor loads the descriptor. Missing file → os.ErrNotExist.
func readDescriptor(p paths.Paths) (*api.Descriptor, error) {
	raw, err := os.ReadFile(p.DaemonDescriptor())
	if err != nil {
		return nil, err
	}
	var d api.Descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("descriptor decode: %w", err)
	}
	return &d, nil
}

// removeOwnDescriptor removes daemon.json only when it still belongs to
// this daemon instance — a stale cleanup must never unlink another
// generation's descriptor.
func removeOwnDescriptor(p paths.Paths, instanceID string) {
	d, err := readDescriptor(p)
	if err != nil {
		return // missing or unreadable: nothing safe to remove
	}
	if d.InstanceID != instanceID {
		return // another generation owns it now
	}
	_ = os.Remove(p.DaemonDescriptor())
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

// readRegular reads a bounded regular file (descriptor must not be a
// symlink or special file).
func readRegular(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}
