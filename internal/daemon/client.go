// Client side of the daemon control plane: one connection, one request,
// one response, close.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/protocol"
)

// ErrNotRunning means no daemon is reachable at the resolved socket.
var ErrNotRunning = errors.New("daemon not running")

// StartupTimeout bounds how long a client waits for a freshly spawned
// daemon to answer its first ping.
const StartupTimeout = 4 * time.Second

const dialTimeout = 2 * time.Second

// Client speaks the control protocol to one daemon socket.
type Client struct {
	sock string
}

// Dial connects and verifies the daemon by pinging it. Returns
// ErrNotRunning when no live daemon answers.
func Dial(p paths.Paths) (*Client, error) {
	c := &Client{sock: p.DaemonSocket()}
	if _, err := c.Ping(); err != nil {
		return nil, ErrNotRunning
	}
	return c, nil
}

// Do performs one request/response round-trip on a fresh connection.
func (c *Client) Do(req protocol.Request) (*protocol.Response, error) {
	conn, err := net.DialTimeout("unix", c.sock, dialTimeout)
	if err != nil {
		return nil, ErrNotRunning
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ReadTimeout + WriteTimeout))

	req.Version = protocol.Version
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	// Half-close so the daemon sees request EOF instead of waiting for more.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if resp.Version != protocol.Version {
		return nil, fmt.Errorf("protocol mismatch: response v%d", resp.Version)
	}
	return &resp, nil
}

// Ping proves daemon identity: PID + protocol version.
func (c *Client) Ping() (*protocol.DaemonInfo, error) {
	r, err := c.Do(protocol.Request{Op: protocol.OpPing})
	if err != nil {
		return nil, err
	}
	return r.Daemon, nil
}

// Ensure returns a client for the state root's daemon, spawning a hidden
// `__daemon` process when none is running. Multiple racing clients may each
// spawn a contender; exactly one wins the daemon lock and the rest exit —
// all clients converge on the same socket/PID.
func Ensure(p paths.Paths, selfExe string) (*Client, error) {
	c := &Client{sock: p.DaemonSocket()}
	if _, err := c.Ping(); err == nil {
		return c, nil
	}
	if err := spawnDaemon(p, selfExe); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(StartupTimeout)
	for time.Now().Before(deadline) {
		if _, err := c.Ping(); err == nil {
			return c, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not become ready within %s", StartupTimeout)
}

// spawnDaemon starts `selfExe __daemon` detached in its own session with
// the resolved absolute REPOSUITE_HOME. It returns immediately; readiness
// is proven by polling the socket.
func spawnDaemon(p paths.Paths, selfExe string) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	logPath := p.LogDir() + "/relayd.log"
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("daemon log: %w", err)
	}
	defer logf.Close()
	_ = os.Chmod(logPath, 0o600)

	cmd := exec.Command(selfExe, "__daemon")
	cmd.Env = append(os.Environ(), "REPOSUITE_HOME="+p.RepoSuiteRoot())
	cmd.Dir = "/"
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	go cmd.Wait() // reap
	return nil
}
