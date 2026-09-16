package client

// Descriptor validation and fail-closed peer behavior: clients never send
// commands to an unknown peer and never auto-start against one.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, d := range []string{home, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return p
}

func validDescriptor(endpoint string) api.Descriptor {
	return api.Descriptor{
		Version:     api.DescriptorVersion,
		InstanceID:  strings.Repeat("a", 32),
		PID:         1234,
		Endpoint:    endpoint,
		BearerToken: strings.Repeat("b", 64),
	}
}

func writeDesc(t *testing.T, p paths.Paths, d api.Descriptor) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DaemonDescriptor(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDescriptorRejects(t *testing.T) {
	cases := map[string]api.Descriptor{
		"version":     {Version: 99, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://127.0.0.1:1", BearerToken: strings.Repeat("b", 64)},
		"instanceId":  {Version: 1, InstanceID: "short", PID: 1, Endpoint: "http://127.0.0.1:1", BearerToken: strings.Repeat("b", 64)},
		"pid":         {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 0, Endpoint: "http://127.0.0.1:1", BearerToken: strings.Repeat("b", 64)},
		"token":       {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://127.0.0.1:1", BearerToken: "xyz"},
		"https":       {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "https://127.0.0.1:1", BearerToken: strings.Repeat("b", 64)},
		"hostname":    {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://localhost:1", BearerToken: strings.Repeat("b", 64)},
		"any address": {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://0.0.0.0:1", BearerToken: strings.Repeat("b", 64)},
		"lan":         {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://192.168.1.5:1", BearerToken: strings.Repeat("b", 64)},
		"no port":     {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://127.0.0.1", BearerToken: strings.Repeat("b", 64)},
		"path":        {Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1, Endpoint: "http://127.0.0.1:1/x", BearerToken: strings.Repeat("b", 64)},
	}
	for name, d := range cases {
		dd := d
		if err := ValidateDescriptor(&dd); !errors.Is(err, ErrBadPeer) {
			t.Fatalf("%s: want ErrBadPeer, got %v", name, err)
		}
	}
	if err := ValidateDescriptor(&api.Descriptor{
		Version: 1, InstanceID: strings.Repeat("a", 32), PID: 1,
		Endpoint: "http://127.0.0.1:43127", BearerToken: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}
}

// TestDialNotRunning: missing descriptor → ErrNotRunning.
func TestDialNotRunning(t *testing.T) {
	p := testPaths(t)
	if _, err := Dial(p); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
}

// TestForeignPeerFailsClosed: a live HTTP peer whose responses are not a
// compatible relayd → ErrBadPeer; Ensure must NOT spawn against it.
func TestForeignPeerFailsClosed(t *testing.T) {
	p := testPaths(t)

	// A plain HTTP server pretending to be the endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello, I am not a relayd\n"))
	}))
	defer srv.Close()
	writeDesc(t, p, validDescriptor(srv.URL))

	var spawned int32
	old := spawnFunc
	spawnFunc = func(string) error { atomic.AddInt32(&spawned, 1); return nil }
	defer func() { spawnFunc = old }()

	if _, err := Dial(p); !errors.Is(err, ErrBadPeer) {
		t.Fatalf("Dial: want ErrBadPeer, got %v", err)
	}
	if _, err := Ensure(p, "/unused"); !errors.Is(err, ErrBadPeer) {
		t.Fatalf("Ensure: want ErrBadPeer, got %v", err)
	}
	if n := atomic.LoadInt32(&spawned); n != 0 {
		t.Fatalf("Ensure spawned %d daemons against a bad peer", n)
	}
}

// TestWrongTokenPeerFailsClosed: an endpoint that answers 401 is a peer
// whose descriptor token does not match — fail closed, never spawn.
func TestWrongTokenPeerFailsClosed(t *testing.T) {
	p := testPaths(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"nope"}}`))
	}))
	defer srv.Close()
	writeDesc(t, p, validDescriptor(srv.URL))

	var spawned int32
	old := spawnFunc
	spawnFunc = func(string) error { atomic.AddInt32(&spawned, 1); return nil }
	defer func() { spawnFunc = old }()

	if _, err := Ensure(p, "/unused"); !errors.Is(err, ErrBadPeer) {
		t.Fatalf("want ErrBadPeer, got %v", err)
	}
	if n := atomic.LoadInt32(&spawned); n != 0 {
		t.Fatalf("Ensure spawned %d daemons against a foreign peer", n)
	}
}

// TestInstanceMismatchFailsClosed: the endpoint answers but belongs to a
// different daemon generation than the descriptor.
func TestInstanceMismatchFailsClosed(t *testing.T) {
	p := testPaths(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"daemon":{"instanceId":"` + strings.Repeat("c", 32) +
			`","pid":5,"apiVersion":1,"uptimeSeconds":1,"sessionCount":0}}`))
	}))
	defer srv.Close()
	writeDesc(t, p, validDescriptor(srv.URL))
	if _, err := Dial(p); !errors.Is(err, ErrBadPeer) {
		t.Fatalf("want ErrBadPeer, got %v", err)
	}
}

// TestDeadEndpointSpawnsContender: a descriptor whose endpoint refuses
// connections is stale — Ensure spawns a contender (the lock owner will
// replace the descriptor).
func TestDeadEndpointSpawnsContender(t *testing.T) {
	p := testPaths(t)
	// Reserve a port then close it: guaranteed-dead loopback endpoint.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + ln.Addr().String()
	ln.Close()
	writeDesc(t, p, validDescriptor(endpoint))

	var spawned int32
	old := spawnFunc
	spawnFunc = func(string) error { atomic.AddInt32(&spawned, 1); return nil }
	defer func() { spawnFunc = old }()

	// Ensure will spawn repeatedly and time out — the point is that it
	// classified the dead endpoint as "not running", not "bad peer".
	if _, err := Ensure(p, "/unused"); !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("want ErrStartTimeout, got %v", err)
	}
	if n := atomic.LoadInt32(&spawned); n < 1 {
		t.Fatal("Ensure did not spawn against a dead endpoint")
	}
}

// TestSilentPeerDeadline: a peer that accepts but never responds hits the
// client-side response-header deadline — no unbounded wait.
func TestSilentPeerDeadline(t *testing.T) {
	p := testPaths(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { // hold the connection open, never reply
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	writeDesc(t, p, validDescriptor("http://"+ln.Addr().String()))

	start := time.Now()
	_, err = Dial(p)
	if err == nil {
		t.Fatal("silent peer must produce an error")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("client deadline not enforced: %s", elapsed)
	}
}
