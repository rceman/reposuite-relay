package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
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

// bindListener binds the loopback control endpoint. The OS chooses the
// ephemeral port; anything that is not loopback fails startup closed.
func (d *Daemon) bindListener() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback control endpoint: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("listener %s is not loopback; refusing startup", ln.Addr())
	}
	d.ln = ln
	return nil
}
