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

// ErrNotRunning means no daemon is reachable at the resolved socket —
// absent socket, refused connection, or dial timeout. It is distinct from
// a live-but-incompatible peer.
var ErrNotRunning = errors.New("daemon not running")

// ErrProtocolMismatch means a live peer answered but speaks an
// incompatible protocol version — never misreported as "not running" and
// never a reason to spawn a second daemon.
var ErrProtocolMismatch = errors.New("incompatible Relay daemon protocol")

// ErrBadPeer means a live socket endpoint did not return a valid protocol
// response at all.
var ErrBadPeer = errors.New("unrecognized daemon peer")

// StartupTimeout bounds how long a client waits for a freshly spawned
// daemon to answer its first ping.
const StartupTimeout = 4 * time.Second

const dialTimeout = 2 * time.Second

// spawnDaemonFunc is the test seam for "Ensure must not spawn" proofs.
var spawnDaemonFunc = spawnDaemon

// Client speaks the control protocol to one daemon socket.
type Client struct {
	sock         string
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// newClient applies production deadlines.
func newClient(sock string) *Client {
	return &Client{sock: sock, readTimeout: ReadTimeout, writeTimeout: WriteTimeout}
}

// Dial connects and verifies the daemon by pinging it. Errors are typed:
// ErrNotRunning when nothing answers, ErrProtocolMismatch for a live
// incompatible peer, ErrBadPeer for a live non-protocol endpoint.
func Dial(p paths.Paths) (*Client, error) {
	c := newClient(p.DaemonSocket())
	if _, err := c.Ping(); err != nil {
		return nil, err
	}
	return c, nil
}

// Do performs one request/response round-trip on a fresh connection with
// phase-specific deadlines: WriteTimeout covers the send phase,
// ReadTimeout covers the receive phase.
func (c *Client) Do(req protocol.Request) (*protocol.Response, error) {
	conn, err := net.DialTimeout("unix", c.sock, dialTimeout)
	if err != nil {
		return nil, ErrNotRunning
	}
	defer conn.Close()

	req.Version = protocol.Version
	_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPeer, err)
	}
	// Half-close so the daemon sees request EOF instead of waiting for more.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(c.readTimeout))
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPeer, err)
	}
	if resp.Version != protocol.Version {
		return nil, fmt.Errorf("%w: client v%d, daemon v%d",
			ErrProtocolMismatch, protocol.Version, resp.Version)
	}
	if !resp.OK && resp.Code == protocol.ErrProtocolMismatch {
		return nil, fmt.Errorf("%w: %s", ErrProtocolMismatch, resp.Error)
	}
	return &resp, nil
}

// Ping proves daemon identity: PID + protocol version.
func (c *Client) Ping() (*protocol.DaemonInfo, error) {
	r, err := c.Do(protocol.Request{Op: protocol.OpPing})
	if err != nil {
		return nil, err
	}
	if r.Daemon == nil {
		return nil, ErrBadPeer
	}
	return r.Daemon, nil
}

// Ensure returns a client for the state root's daemon, spawning a hidden
// `__daemon` process only when nothing is running. A live but incompatible
// or malformed peer fails immediately — spawning another daemon against it
// would be wrong (it may still hold relayd.lock).
func Ensure(p paths.Paths, selfExe string) (*Client, error) {
	c := newClient(p.DaemonSocket())
	_, err := c.Ping()
	switch {
	case err == nil:
		return c, nil
	case !errors.Is(err, ErrNotRunning):
		return nil, err // protocol mismatch / bad peer: never spawn
	}
	if err := spawnDaemonFunc(p, selfExe); err != nil {
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
