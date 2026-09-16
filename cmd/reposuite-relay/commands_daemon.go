package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rceman/reposuite-relay/internal/client"
)

// cmdDaemon implements `daemon status|stop` — neither auto-starts a
// daemon.
func cmdDaemon(args []string) int {
	if len(args) != 1 || (args[0] != "status" && args[0] != "stop") {
		fmt.Fprintln(os.Stderr, "usage: reposuite-relay daemon {status|stop}")
		return 2
	}
	p, code, ok := pathsForClient()
	if !ok {
		return code
	}
	c, err := client.Dial(p)
	if err != nil {
		if errors.Is(err, client.ErrNotRunning) {
			fmt.Fprintln(os.Stderr, "reposuite-relay: daemon not running")
		} else {
			// Bad peer / incompatible endpoint is NOT "not running".
			fmt.Fprintln(os.Stderr, "reposuite-relay:", err)
		}
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if args[0] == "status" {
		info, err := c.DaemonInfo(ctx)
		if err != nil {
			return failAPI(err)
		}
		fmt.Printf("pid=%d apiVersion=%d uptimeSeconds=%.3f sessionCount=%d\n",
			info.PID, info.APIVersion, info.UptimeSeconds, info.SessionCount)
		return 0
	}
	// stop: request shutdown, then wait bounded for the descriptor to go
	// away (the lock owner removes its own descriptor as it exits).
	if _, err := c.Shutdown(ctx); err != nil {
		return failAPI(err)
	}
	desc := p.DaemonDescriptor()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(desc); os.IsNotExist(err) {
			fmt.Println("daemon stopped")
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "reposuite-relay: daemon did not exit within timeout")
	return 1
}
