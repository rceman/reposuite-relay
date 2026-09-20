// Package config owns the persistent Relay control-plane configuration:
// <relay>/config/relay.json — the stable loopback endpoint selected once
// at first successful start and reused on every later start.
//
// The file is durable configuration, not process state: it lives under
// config/, never under run/. It is written once — atomically — by the
// daemon singleton owner, and read (never rewritten) by everything else.
// Malformed or unsupported content fails closed; Relay never silently
// rewrites a configured port.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SchemaVersion is the only supported relay.json schema.
const SchemaVersion = 1

// ListenHost is the only host Relay binds: loopback, always.
const ListenHost = "127.0.0.1"

// maxFile bounds a config file read — the schema is tiny, so anything
// larger is malformed by definition.
const maxFile = 8 << 10

// Config is the persistent control-plane configuration.
type Config struct {
	SchemaVersion int    `json:"schemaVersion"`
	ListenHost    string `json:"listenHost"`
	ListenPort    int    `json:"listenPort"`
}

// Address is the bind address for the configured endpoint.
func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.ListenHost, c.ListenPort)
}

// Validate fails closed on anything that is not exactly the supported
// loopback endpoint schema.
func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d (want %d)", c.SchemaVersion, SchemaVersion)
	}
	if c.ListenHost != ListenHost {
		return fmt.Errorf("listenHost %q unsupported; Relay binds %s only", c.ListenHost, ListenHost)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("listenPort %d out of range 1..65535", c.ListenPort)
	}
	return nil
}

// Load reads and strictly validates the config. Missing file →
// os.ErrNotExist (unconfigured is a state, not an error). Malformed or
// unsupported content is a hard error — the caller must fail closed.
func Load(path string) (Config, error) {
	raw, err := readRegular(path, maxFile)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("relay config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("relay config %s: %w", path, err)
	}
	return c, nil
}

// Commit persists the config atomically (same-directory temp, file sync,
// atomic rename, directory sync) with 0600 permissions. It is the single
// commit point for the first-start port selection: the caller must only
// invoke it under singleton ownership and after the port is bound.
func Commit(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("relay config: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".relay.json.tmp-*")
	if err != nil {
		return fmt.Errorf("relay config temp: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("relay config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
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
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("relay config rename: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("relay config dir sync: %w", err)
	}
	return nil
}

// readRegular reads a bounded regular file (config must not be a symlink
// or special file).
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

// syncDir fsyncs a directory so entry changes are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
