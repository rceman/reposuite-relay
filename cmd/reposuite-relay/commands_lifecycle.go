package main

// Canonical local lifecycle (serve/start/stop/restart). `status` lives in
// commands_status.go. All commands reuse the existing primitives:
// daemon.Run for the foreground process, client.Ensure for detached
// spawn + readiness, and the authenticated /v1/daemon/shutdown for stop.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/daemon"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// cmdServe runs the production daemon in the FOREGROUND — the canonical
// entrypoint for external supervisors (systemd/launchd/Docker/terminal)
// and for the detached `start` spawn. Relay never installs or registers
// any of those; the contract is simply `reposuite-relay serve`.
func cmdServe(selfExe string) int {
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	if err := daemon.Run(p, selfExe); err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			fmt.Fprintln(os.Stderr, "reposuite-relay: a daemon already owns this state root")
			return 1
		}
		return fail(err)
	}
	return 0
}

// cmdStart ensures one detached daemon is running and reports it. It is
// idempotent: a live daemon is reported, never duplicated — the singleton
// lock plus descriptor validation make a second instance impossible.
func cmdStart(selfExe string) int {
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	if _, err := client.Dial(p); err == nil {
		fmt.Println("Relay already running")
		printDaemonInfo(p)
		return 0
	} else if !errors.Is(err, client.ErrNotRunning) && !errors.Is(err, client.ErrStaleDescriptor) {
		// A bad peer at the endpoint is not something to spawn against.
		return fail(err)
	}
	if _, err := client.Ensure(p, selfExe); err != nil {
		fmt.Fprintf(os.Stderr, "reposuite-relay: %v (see %s)\n",
			err, daemonLogPath(p))
		return 1
	}
	fmt.Println("Relay started")
	printDaemonInfo(p)
	return 0
}

// cmdStop gracefully stops the daemon — authenticated shutdown request,
// then a bounded wait for the owner to remove its descriptor (which
// happens only after the full quiescence drain). Idempotent: an absent
// daemon reports already-stopped success.
func cmdStop() int {
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	return stopDaemon(p, false)
}

// cmdRestart is composition, not a second lifecycle engine: stop if
// running, then start and wait for readiness.
func cmdRestart(selfExe string) int {
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	if code := stopDaemon(p, true); code != 0 {
		return code
	}
	if _, err := client.Ensure(p, selfExe); err != nil {
		fmt.Fprintf(os.Stderr, "reposuite-relay: %v (see %s)\n",
			err, daemonLogPath(p))
		return 1
	}
	fmt.Println("Relay restarted")
	printDaemonInfo(p)
	return 0
}

// stopDaemon requests graceful shutdown and waits bounded for the
// descriptor to disappear — the lock owner removes it only after the
// quiescence drain (sessions stay durable; runtimes stop). Already-stopped
// and stale-descriptor states are success.
func stopDaemon(p paths.Paths, quiet bool) int {
	// Dial, tolerating a mid-transition descriptor: ErrStaleDescriptor
	// means the endpoint is a NEWER live relayd generation than the file —
	// the owner will replace it shortly, so re-dial briefly rather than
	// report a false "stopped".
	var c *client.Client
	var err error
	dialDeadline := time.Now().Add(2 * time.Second)
	for {
		c, err = client.Dial(p)
		if !errors.Is(err, client.ErrStaleDescriptor) || time.Now().After(dialDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			if !quiet {
				fmt.Println("Relay already stopped")
			}
			return 0
		}
		// Bad peer / incompatible endpoint is NOT "not running".
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Shutdown(ctx); err != nil {
		return failAPI(err)
	}
	desc := p.DaemonDescriptor()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(desc); os.IsNotExist(err) {
			fmt.Println("Relay stopped")
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "reposuite-relay: daemon did not exit within timeout")
	return 1
}

// printDaemonInfo reports the live daemon's operator facts — PID and the
// Web Admin URL — from an authenticated info request plus the persisted
// endpoint. Best-effort: a race against shutdown degrades to no report.
func printDaemonInfo(p paths.Paths) {
	c, err := client.Dial(p)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	info, err := c.DaemonInfo(ctx)
	cancel()
	if err != nil {
		return
	}
	fmt.Printf("PID:  %d\n", info.PID)
	if cfg, err := config.Load(p.RelayConfig()); err == nil {
		fmt.Printf("Web:  http://%s/\n", cfg.Address())
	}
}

// daemonLogPath is where a detached daemon's stdio goes — the diagnostic
// destination reported when `start`/`restart` fails readiness.
func daemonLogPath(p paths.Paths) string {
	return p.DaemonLog()
}
