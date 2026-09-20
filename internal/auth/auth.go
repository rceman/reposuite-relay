// Package auth owns the persistent machine API credential:
// <relay>/config/api.token — one opaque 256-bit bearer for trusted
// machine clients (GPT Tunnel, automation, future Web Admin).
//
// It is a credential domain separate from the ephemeral daemon
// descriptor bearer: created once under daemon singleton ownership,
// loaded and strictly validated on every start, never published in the
// descriptor, never returned by any API, never logged, and never printed
// by any command. Malformed content fails closed — Relay never silently
// replaces a credential.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tokenBytes is the credential entropy: 256 bits, hex-encoded to 64
// characters on disk.
const tokenBytes = 32

// maxFile bounds a credential read — the canonical file is 65 bytes
// (64 hex + newline); anything beyond this bound is malformed, and an
// oversized file is rejected rather than truncated to a valid prefix.
const maxFile = 128

// Generate returns a fresh 256-bit bearer token (64 lowercase hex).
func Generate() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Valid reports whether s is a well-formed machine token.
func Valid(s string) bool {
	if len(s) != tokenBytes*2 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// Load reads and strictly validates the machine token. Missing file →
// os.ErrNotExist; malformed content is a hard error — fail closed.
func Load(path string) (string, error) {
	raw, err := readRegular(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimRight(string(raw), "\r\n")
	if !Valid(tok) {
		return "", fmt.Errorf("machine token %s: malformed credential", path)
	}
	return tok, nil
}

// Ensure returns the canonical machine token, creating it atomically
// (0600) when absent. It must be called only under daemon singleton
// ownership — concurrent contenders would otherwise race to mint
// competing canonical tokens.
func Ensure(path string) (string, error) {
	tok, err := Load(path)
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err // malformed: fail closed, never replace
	}
	tok, err = Generate()
	if err != nil {
		return "", err
	}
	if err := commit(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// commit persists the token atomically (same-directory temp, file sync,
// atomic rename, directory sync) with 0600 permissions.
func commit(path, tok string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".api.token.tmp-*")
	if err != nil {
		return fmt.Errorf("machine token temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("machine token: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("machine token rename: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("machine token dir sync: %w", err)
	}
	return nil
}

// readRegular reads a bounded, owner-private regular file: a symlink or
// special file, any mode other than exactly 0600, or content exceeding
// the bound all fail closed — a truncated prefix is never parsed as a
// valid credential, and an already-exposed secret is never silently
// repaired.
func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("machine token %s: mode %o, want 0600 owner-private",
			path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFile {
		return nil, fmt.Errorf("machine token %s: exceeds %d-byte bound", path, maxFile)
	}
	return raw, nil
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
