// Package client is the single local HTTP client for relayd (ADR-006):
// descriptor resolution and validation, bearer authentication, bounded
// requests, stable API error decoding, and typed methods. The CLI (and
// later the TUI and Gateway bridge) must go through this package — HTTP
// request construction is never duplicated.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// Stable client-side errors.
var (
	// ErrNotRunning means no live daemon answers at the descriptor
	// endpoint (or no descriptor exists).
	ErrNotRunning = errors.New("relayd is not running")
	// ErrBadPeer means the descriptor or its endpoint is reachable but is
	// not a compatible relayd. Clients fail closed — they never send
	// commands to an unknown peer.
	ErrBadPeer = errors.New("daemon descriptor does not describe a compatible relayd")
	// ErrStartTimeout means an auto-start attempt did not produce a ready
	// daemon within the deadline.
	ErrStartTimeout = errors.New("relayd did not become ready in time")
)

// APIError is a decoded stable API error.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// StartTimeout bounds Ensure's wait for a spawned daemon to publish a
// ready descriptor.
const StartTimeout = 8 * time.Second

const pingTimeout = 2 * time.Second

// Client talks to one daemon generation.
type Client struct {
	desc api.Descriptor
	hc   *http.Client
}

// Endpoint returns the daemon's loopback endpoint.
func (c *Client) Endpoint() string { return c.desc.Endpoint }

// InstanceID returns the daemon generation identity from the descriptor.
func (c *Client) InstanceID() string { return c.desc.InstanceID }

// ReadDescriptor loads and validates the runtime descriptor.
func ReadDescriptor(p paths.Paths) (*api.Descriptor, error) {
	raw, err := os.ReadFile(p.DaemonDescriptor())
	if err != nil {
		return nil, err
	}
	var d api.Descriptor
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: descriptor decode: %v", ErrBadPeer, err)
	}
	if err := ValidateDescriptor(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ValidateDescriptor fails closed on anything that is not exactly a
// loopback relayd descriptor of a supported version.
func ValidateDescriptor(d *api.Descriptor) error {
	if d.Version != api.DescriptorVersion {
		return fmt.Errorf("%w: unsupported descriptor version %d", ErrBadPeer, d.Version)
	}
	if len(d.InstanceID) != 32 || !isHex(d.InstanceID) {
		return fmt.Errorf("%w: malformed instanceId", ErrBadPeer)
	}
	if d.PID <= 0 {
		return fmt.Errorf("%w: malformed pid", ErrBadPeer)
	}
	if len(d.BearerToken) != 64 || !isHex(d.BearerToken) {
		return fmt.Errorf("%w: malformed bearer token", ErrBadPeer)
	}
	u, err := url.Parse(d.Endpoint)
	if err != nil {
		return fmt.Errorf("%w: malformed endpoint", ErrBadPeer)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("%w: endpoint scheme %q is not http", ErrBadPeer, u.Scheme)
	}
	if u.Hostname() != "127.0.0.1" {
		return fmt.Errorf("%w: endpoint host %q is not loopback 127.0.0.1", ErrBadPeer, u.Hostname())
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("%w: endpoint port invalid", ErrBadPeer)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%w: endpoint must not carry a path", ErrBadPeer)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: endpoint must not carry a query", ErrBadPeer)
	}
	return nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			// Loopback only — no proxies, no keep-alive surprises.
			Proxy:                 nil,
			MaxIdleConnsPerHost:   4,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
}

// Dial connects to a running daemon without ever starting one: the
// descriptor must exist, validate, and answer an authenticated ping.
// `daemon status` / `daemon stop` use this and must not auto-start.
func Dial(p paths.Paths) (*Client, error) {
	d, err := ReadDescriptor(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	c := &Client{desc: *d, hc: newHTTPClient()}
	if err := c.ping(); err != nil {
		return nil, err
	}
	return c, nil
}

// ping performs the authenticated readiness check.
func (c *Client) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodGet, "/v1/daemon", nil, &resp)
	switch {
	case err == nil:
		if resp.Daemon.APIVersion != api.Version {
			return fmt.Errorf("%w: endpoint speaks API version %d", ErrBadPeer, resp.Daemon.APIVersion)
		}
		if resp.Daemon.InstanceID != c.desc.InstanceID {
			// The endpoint belongs to a different generation than the
			// descriptor we read — a stale or foreign peer.
			return fmt.Errorf("%w: endpoint instance does not match descriptor", ErrBadPeer)
		}
		return nil
	case errors.Is(err, ErrNotRunning):
		return err
	default:
		var ae *APIError
		if errors.As(err, &ae) {
			if ae.Status == http.StatusUnauthorized {
				return fmt.Errorf("%w: endpoint rejected the descriptor token", ErrBadPeer)
			}
			if ae.Status == http.StatusServiceUnavailable {
				return ErrNotRunning // shutting down — a new generation is coming
			}
			return fmt.Errorf("%w: %v", ErrBadPeer, err)
		}
		if errors.Is(err, ErrBadPeer) {
			// The endpoint answered, but not as a compatible relayd.
			return err
		}
		// Connection refused / dial failure: the descriptor is stale.
		return ErrNotRunning
	}
}

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
		// Not running yet. The spawned contender may have lost the lock
		// race or the previous generation may still be shutting down —
		// spawn another (bounded by the overall deadline; contenders exit
		// immediately when they lose the lock).
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

// --- request plumbing -------------------------------------------------

// do performs one authenticated bounded request. Connection-level
// failures are classified as ErrNotRunning (daemon gone).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.desc.Endpoint+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.desc.BearerToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var eb api.ErrorBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Code != "" {
			return &APIError{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
		}
		return &APIError{
			Status:  resp.StatusCode,
			Code:    fmt.Sprintf("HTTP_%d", resp.StatusCode),
			Message: strings.TrimSpace(string(raw)),
		}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return fmt.Errorf("%w: decode response: %v", ErrBadPeer, err)
		}
	}
	return nil
}

func sessionPath(key string) string { return "/v1/sessions/" + url.PathEscape(key) }

// --- typed methods ----------------------------------------------------

// DaemonInfo returns the daemon generation info.
func (c *Client) DaemonInfo(ctx context.Context) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodGet, "/v1/daemon", nil, &resp)
	return resp.Daemon, err
}

// Sessions lists durable sessions (COLD sessions included).
func (c *Client) Sessions(ctx context.Context) (api.SessionList, error) {
	var resp api.SessionList
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &resp)
	return resp, err
}

// ServeFixture creates a fixture session.
func (c *Client) ServeFixture(ctx context.Context, key, cwd string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/sessions/fixture",
		api.FixtureRequest{Key: key, Cwd: cwd}, &resp)
	return resp, err
}

// CreateCodex creates a durable Codex session (COLD — no app-server is
// started until the first prompt).
func (c *Client) CreateCodex(ctx context.Context, key, cwd, model, mode string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/sessions/codex",
		api.CodexRequest{Key: key, Cwd: cwd, Model: model, Mode: mode}, &resp)
	return resp, err
}

// Prompt submits a user prompt as a native turn (202 Accepted).
func (c *Client) Prompt(ctx context.Context, key, text, model, effort string) (api.PromptResponse, error) {
	var resp api.PromptResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/prompt",
		api.PromptRequest{Text: text, Model: model, Effort: effort}, &resp)
	return resp, err
}

// Cancel interrupts the in-flight native turn.
func (c *Client) Cancel(ctx context.Context, key string) (api.CancelResponse, error) {
	var resp api.CancelResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/cancel", nil, &resp)
	return resp, err
}

// AnswerInput answers a native requested-input request.
func (c *Client) AnswerInput(ctx context.Context, key, inputID string, answers []api.InputAnswer) (api.InputResponse, error) {
	var resp api.InputResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/input",
		api.InputRequest{InputID: inputID, Answers: answers}, &resp)
	return resp, err
}

// SetConfig accepts a model/mode change for a Codex session.
func (c *Client) SetConfig(ctx context.Context, key string, model, mode *string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPatch, sessionPath(key)+"/config",
		api.ConfigRequest{Model: model, Mode: mode}, &resp)
	return resp, err
}

// Status returns one session.
func (c *Client) Status(ctx context.Context, key string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodGet, sessionPath(key), nil, &resp)
	return resp, err
}

// StopSession deletes a session (runtime stop + durable delete).
func (c *Client) StopSession(ctx context.Context, key string) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodDelete, sessionPath(key), nil, &resp)
	return resp.Daemon, err
}

// Shutdown asks the daemon to stop gracefully.
func (c *Client) Shutdown(ctx context.Context) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodPost, "/v1/daemon/shutdown", nil, &resp)
	return resp.Daemon, err
}

// Transcript fetches the bounded durable transcript tail. The limit is
// always sent explicitly: limit 0 means "no records", and the server
// rejects negative limits.
func (c *Client) Transcript(ctx context.Context, key string, limit int) (api.TranscriptPage, error) {
	var resp api.TranscriptPage
	path := sessionPath(key) + "/transcript?limit=" + strconv.Itoa(limit)
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}

// EventStream is an open NDJSON event stream.
type EventStream struct {
	rc io.ReadCloser
	sc *bufio.Scanner
}

// Next returns the next canonical event, or an error: io.EOF at stream
// end, *APIError for a deterministic stream termination
// (SUBSCRIBER_EVICTED / STREAM_CLOSED), or a transport error.
func (s *EventStream) Next() (events.Event, error) {
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe struct {
			Error *api.ErrorDetail `json:"error"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Error != nil {
			return events.Event{}, &APIError{Code: probe.Error.Code, Message: probe.Error.Message}
		}
		var ev events.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return events.Event{}, fmt.Errorf("malformed event line: %w", err)
		}
		return ev, nil
	}
	if err := s.sc.Err(); err != nil {
		return events.Event{}, err
	}
	return events.Event{}, io.EOF
}

// Close ends the stream (cancels the subscription server-side).
func (s *EventStream) Close() error { return s.rc.Close() }

// Events opens the canonical event stream for a session after the given
// cursor. A too-old cursor is returned as *APIError CURSOR_TOO_OLD before
// any streaming begins.
func (c *Client) Events(ctx context.Context, key string, after uint64) (*EventStream, error) {
	path := sessionPath(key) + "/events?after=" + strconv.FormatUint(after, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.desc.Endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.desc.BearerToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var eb api.ErrorBody
		if json.Unmarshal(raw, &eb) == nil && eb.Error.Code != "" {
			return nil, &APIError{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
		}
		return nil, &APIError{Status: resp.StatusCode, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode)}
	}
	sc := bufio.NewScanner(resp.Body)
	// The scanner accepts exactly the canonical frame bound — the same
	// contract enforced at publication and by the NDJSON server.
	sc.Buffer(make([]byte, 0, 64*1024), api.MaxEventFrameBytes)
	return &EventStream{rc: resp.Body, sc: sc}, nil
}

// ExecutablePath is a small helper for tests/spawn wiring.
func ExecutablePath() (string, error) { return filepath.Abs(os.Args[0]) }
