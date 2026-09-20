// Auto-start path for managed commands: spawn a detached __daemon
// contender when no live daemon answers, then converge on the ready
// descriptor. Split from client.go to keep the dial/descriptor core
// inside the token budget.
package client

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/rceman/reposuite-relay/internal/paths"
)

// StartTimeout bounds Ensure's wait for a spawned daemon to publish a
// ready descriptor.
const StartTimeout = 8 * time.Second

// Ensure returns a client for a live daemon, auto-starting relayd when it
// is genuinely absent (missing or dead descriptor). A descriptor whose
// endpoint answers but is not a compatible authenticated relayd fails
// closed as ErrBadPeer — commands are never sent to an unknown peer.
// Clients never remove stale descriptors; the daemon lock owner owns
// descriptor replacement.
func Ensure(p paths.Paths, selfExe string) (*Client, error) {
	if selfExe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate executable: %w", err)
		}
		selfExe = exe
	}
	deadline := time.Now().Add(StartTimeout)
	lastSpawn := time.Time{}
	spawn := func() error {
		lastSpawn = time.Now()
		return spawnFunc(selfExe)
	}
	// Fast path: live daemon already.
	if c, err := Dial(p); err == nil {
		return c, nil
	} else if errors.Is(err, ErrBadPeer) {
		return nil, err
	}
	if err := spawn(); err != nil {
		return nil, err
	}
	for time.Now().Before(deadline) {
		c, err := Dial(p)
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, ErrBadPeer):
			return nil, err // fail closed, never retry into a bad peer
		}
		// Not running yet — or a stale-descriptor read raced the new
		// generation's publication on the stable port. The spawned
		// contender may have lost the lock race or the previous generation
		// may still be shutting down — spawn another (bounded by the
		// overall deadline; contenders exit when they lose the lock).
		if time.Since(lastSpawn) > 300*time.Millisecond {
			if err := spawn(); err != nil {
				return nil, err
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, ErrStartTimeout
}

// spawnFunc is the auto-start seam (test-only override).
var spawnFunc = spawnDaemon

// spawnDaemon launches a detached __daemon contender. Contenders that
// lose the singleton lock exit immediately; the winner publishes the
// descriptor. Never blocks on the child.
func spawnDaemon(selfExe string) error {
	cmd := exec.Command(selfExe, "__daemon")
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		defer devNull.Close()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start relayd: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}
