// Package client is the single local HTTP client for relayd (ADR-006):
// descriptor resolution and validation, bearer authentication, bounded
// requests, stable API error decoding, and typed methods. The CLI (and
// later external machine integrations like Gateway) must go through this
// package — HTTP request construction is never duplicated.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// ErrStaleDescriptor means the endpoint is a compatible relayd but not
	// the generation our descriptor describes: the descriptor predates a
	// newer generation's publication — transient while the stable port
	// changes hands. Never sent commands against.
	ErrStaleDescriptor = errors.New("daemon descriptor predates the live generation")
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
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
			}).DialContext,
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
//
// The stable endpoint makes one race real: the descriptor we read can
// predate the generation now answering on the configured port (the read
// raced the restart's atomic rename). A serving generation always
// publishes its own descriptor before it accepts, so a token rejection
// resolves by re-reading: a CHANGED descriptor describes the live
// generation — retry it; an UNCHANGED one means the peer genuinely
// rejects the descriptor — fail closed.
func Dial(p paths.Paths) (*Client, error) {
	var lastInstance string
	for i := 0; i < 4; i++ {
		d, err := ReadDescriptor(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrNotRunning
			}
			return nil, err
		}
		c := &Client{
			desc: *d,
			hc:   newHTTPClient(),
		}
		err = c.ping()
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, ErrStaleDescriptor) && d.InstanceID != lastInstance:
			// The descriptor rotated under us — the live generation's
			// own descriptor is now on disk; one more read converges.
			lastInstance = d.InstanceID
			continue
		case errors.Is(err, ErrStaleDescriptor):
			// Same descriptor, still rejected: the peer is not this
			// descriptor's generation and none is coming — fail closed.
			return nil, fmt.Errorf("%w: endpoint rejected the descriptor token", ErrBadPeer)
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: endpoint rejected the descriptor token", ErrBadPeer)
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
				// A relayd-shaped 401: the endpoint is a compatible relayd
				// but our descriptor predates the serving generation.
				return fmt.Errorf("%w: endpoint rejected the descriptor token", ErrStaleDescriptor)
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
			return &APIError{
				Status:  resp.StatusCode,
				Code:    eb.Error.Code,
				Message: eb.Error.Message,
			}
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

// ExecutablePath is a small helper for tests/spawn wiring.
func ExecutablePath() (string, error) { return filepath.Abs(os.Args[0]) }
