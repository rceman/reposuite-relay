package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/rceman/reposuite-relay/internal/config"
)

// acquireLock takes the exclusive non-blocking advisory lock that makes the
// daemon a singleton. Stale lock FILES are harmless: the kernel lock, not
// file existence, is the authority.
func (d *Daemon) acquireLock() error {
	lockPath := d.paths.DaemonLock()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("daemon lock: %w", err)
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		f.Close()
		return fmt.Errorf("daemon lock chmod: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return ErrAlreadyRunning
	}
	d.lockFile = f
	return nil
}

func (d *Daemon) releaseLock() {
	_ = syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN)
	d.lockFile.Close()
}

// resolveEndpoint binds the stable loopback control endpoint under
// singleton authority. Exactly two cases:
//
//   - config/relay.json exists: bind the configured address EXACTLY. A
//     configured port that cannot be bound fails startup — Relay never
//     falls back to another port or rewrites the config.
//   - no config yet (first start): the OS selects a free loopback port,
//     the daemon binds it, and only then does that exact port become
//     durable config through the atomic commit point.
//
// A malformed or unsupported relay.json fails closed: the configured
// endpoint is an identity, never something to guess at.
func (d *Daemon) resolveEndpoint() error {
	cfg, err := config.Load(d.paths.RelayConfig())
	switch {
	case err == nil:
		return d.bindExact(cfg)
	case errors.Is(err, os.ErrNotExist):
		return d.bindAndCommit()
	default:
		return fmt.Errorf("control-plane config: %w", err)
	}
}

// bindExact binds the configured stable endpoint — no fallback, no
// opportunistic re-selection.
func (d *Daemon) bindExact(cfg config.Config) error {
	ln, err := net.Listen("tcp", cfg.Address())
	if err != nil {
		return fmt.Errorf("bind configured control endpoint %s: %w", cfg.Address(), err)
	}
	if err := checkLoopback(ln); err != nil {
		return err
	}
	d.ln = ln
	return nil
}

// bindAndCommit implements first-start port selection: the OS chooses a
// free loopback port, the daemon binds it, and only then does that exact
// port become durable config — atomically. When the commit fails the
// listener is closed: no readiness descriptor is ever published for an
// endpoint that did not become the stable identity. A config committed
// but followed by a later startup failure still stands — the port was
// legitimately selected and committed at this point.
func (d *Daemon) bindAndCommit() error {
	ln, err := net.Listen("tcp", config.ListenHost+":0")
	if err != nil {
		return fmt.Errorf("bind loopback control endpoint: %w", err)
	}
	if err := checkLoopback(ln); err != nil {
		return err
	}
	addr := ln.Addr().(*net.TCPAddr)
	cfg := config.Config{
		SchemaVersion: config.SchemaVersion,
		ListenHost:    config.ListenHost,
		ListenPort:    addr.Port,
	}
	if err := config.Commit(d.paths.RelayConfig(), cfg); err != nil {
		ln.Close()
		return err
	}
	d.ln = ln
	return nil
}

// checkLoopback fails startup closed when the bound address is not
// loopback — Relay never serves a non-loopback endpoint.
func checkLoopback(ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("listener %s is not loopback; refusing startup", ln.Addr())
	}
	return nil
}
