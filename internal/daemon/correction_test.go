package daemon

// Same-package tests for the lifecycle/protocol corrections: they run the
// daemon in-process (Start/Serve/Shutdown) so test seams — spawn counting,
// randomness failure, short deadlines, spawn suppression — are usable
// without a second binary.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/protocol"
	"github.com/rceman/reposuite-relay/internal/session"
)

// startInProcess brings up a daemon on an isolated root with the given
// option overrides. It is Serve'd in a goroutine and Shutdown on cleanup.
func startInProcess(t *testing.T, mutate func(*Options)) (*Daemon, paths.Paths, *Client) {
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
	opts := Options{SelfExe: "/bin/true"} // fixture spawn is overridden in tests
	if mutate != nil {
		mutate(&opts)
	}
	d, err := Start(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve()
	t.Cleanup(d.Shutdown)

	// Wait until the socket answers ping.
	c := newClient(p.DaemonSocket())
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.Ping(); err == nil {
			return d, p, c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-process daemon did not become ready")
	return nil, paths.Paths{}, nil
}

func do(t *testing.T, c *Client, req protocol.Request) *protocol.Response {
	t.Helper()
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do(%s): %v", req.Op, err)
	}
	return r
}

// TestDuplicateKeyNoSpawn: a duplicate serve must create ZERO extra
// fixture processes — the key is reserved before spawn.
func TestDuplicateKeyNoSpawn(t *testing.T) {
	var spawns int32
	_, _, c := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			atomic.AddInt32(&spawns, 1)
			return fixture.Spawn(selfExe, cwd)
		}
	})

	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "same", Cwd: "/"})
	if !r.OK {
		t.Fatalf("first serve failed: %+v", r)
	}
	r = do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "same", Cwd: "/"})
	if r.OK || r.Code != protocol.ErrSessionExists {
		t.Fatalf("duplicate: %+v", r)
	}
	if n := atomic.LoadInt32(&spawns); n != 1 {
		t.Fatalf("spawn count = %d, want 1 — duplicate spawned a process", n)
	}
}

// TestSameKeyCreateRace: 16 concurrent serves for the SAME key — exactly
// one succeeds, one fixture is spawned, one session exists.
func TestSameKeyCreateRace(t *testing.T) {
	var spawns int32
	_, _, c := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			atomic.AddInt32(&spawns, 1)
			return fixture.Spawn(selfExe, cwd)
		}
	})

	const n = 16
	var wg sync.WaitGroup
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := c.Do(protocol.Request{Op: protocol.OpServeFixture, Key: "raced", Cwd: "/"})
			if err != nil {
				codes[i] = "TRANSPORT:" + err.Error()
				return
			}
			if r.OK {
				codes[i] = "OK"
			} else {
				codes[i] = r.Code
			}
		}(i)
	}
	wg.Wait()

	okCount, exists := 0, 0
	for _, code := range codes {
		switch code {
		case "OK":
			okCount++
		case protocol.ErrSessionExists:
			exists++
		default:
			t.Fatalf("unexpected result %q", code)
		}
	}
	if okCount != 1 || exists != n-1 {
		t.Fatalf("ok=%d exists=%d (want 1/%d)", okCount, exists, n-1)
	}
	if n := atomic.LoadInt32(&spawns); n != 1 {
		t.Fatalf("spawn count = %d, want 1", n)
	}
	r := do(t, c, protocol.Request{Op: protocol.OpSessionStatus, Key: "raced"})
	if !r.OK || r.Session == nil {
		t.Fatalf("final session missing: %+v", r)
	}
}

// TestSameKeyStopRace: concurrent stops — exactly one caller owns the stop
// (BeginStop exclusivity), session removed; losers get SESSION_NOT_FOUND.
// Single-invocation of the generation's Stop is proven separately at the
// registry level in TestStopOwnershipSingleInvocation.
func TestSameKeyStopRace(t *testing.T) {
	_, _, c := startInProcess(t, nil)

	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "victim", Cwd: "/"})
	if !r.OK {
		t.Fatalf("serve: %+v", r)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := c.Do(protocol.Request{Op: protocol.OpStopSession, Key: "victim"})
			if err != nil {
				codes[i] = "TRANSPORT:" + err.Error()
				return
			}
			if r.OK {
				codes[i] = "OK"
			} else {
				codes[i] = r.Code
			}
		}(i)
	}
	wg.Wait()

	okCount, notFound := 0, 0
	for _, code := range codes {
		switch code {
		case "OK":
			okCount++
		case protocol.ErrSessionNotFound:
			notFound++
		default:
			t.Fatalf("unexpected %q", code)
		}
	}
	if okCount != 1 || notFound != n-1 {
		t.Fatalf("ok=%d notFound=%d (want 1/%d)", okCount, notFound, n-1)
	}
	r = do(t, c, protocol.Request{Op: protocol.OpSessionStatus, Key: "victim"})
	if r.OK {
		t.Fatalf("session must be gone: %+v", r)
	}
}

// TestStopOwnershipSingleInvocation — registry-level proof that only one
// BeginStop caller can invoke Stop on a generation.
func TestStopOwnershipSingleInvocation(t *testing.T) {
	r := session.NewRegistry()
	var stopCalls int32
	now := time.Now()
	m := &session.Managed{
		Session:    &session.Session{Key: "k", RuntimeID: "x", Harness: session.HarnessFixture, State: session.StateRunning, CreatedAt: now, Generation: 1},
		Generation: &session.ActiveGeneration{PID: 1, StartedAt: now},
		Stop:       func() error { atomic.AddInt32(&stopCalls, 1); return nil },
	}
	if !r.Reserve("k") || !r.Commit("k", m) {
		t.Fatal("setup")
	}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, ok := r.BeginStop("k"); ok {
				_ = got.Stop()
				r.CommitStop("k")
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&stopCalls); n != 1 {
		t.Fatalf("Stop invoked %d times, want 1", n)
	}
	if r.Len() != 0 {
		t.Fatal("session must be gone")
	}
}

// TestRuntimeIDFailure: crypto/rand failure → clean structured failure,
// zero spawns, empty registry, key retryable.
func TestRuntimeIDFailure(t *testing.T) {
	var spawns int32
	failRand := errors.New("no entropy")
	var randFails int32 = 1
	_, _, c := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			atomic.AddInt32(&spawns, 1)
			return fixture.Spawn(selfExe, cwd)
		}
		o.RandID = func() (string, error) {
			if atomic.LoadInt32(&randFails) == 1 {
				return "", failRand
			}
			return session.NewRuntimeID()
		}
	})

	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "k", Cwd: "/"})
	if r.OK || r.Code != protocol.ErrInternal {
		t.Fatalf("want INTERNAL, got %+v", r)
	}
	if n := atomic.LoadInt32(&spawns); n != 0 {
		t.Fatalf("spawn count = %d, want 0 on rand failure", n)
	}
	r = do(t, c, protocol.Request{Op: protocol.OpSessionStatus, Key: "k"})
	if r.OK {
		t.Fatalf("session must not exist: %+v", r)
	}
	// Retry with working randomness succeeds — reservation was released.
	atomic.StoreInt32(&randFails, 0)
	r = do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "k", Cwd: "/"})
	if !r.OK {
		t.Fatalf("retry must succeed: %+v", r)
	}
}

// TestSpawnFailure: fixture spawn error → FIXTURE_START_FAILED, empty
// registry, reservation released, retry works.
func TestSpawnFailure(t *testing.T) {
	var failSpawn int32 = 1
	_, _, c := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			if atomic.LoadInt32(&failSpawn) == 1 {
				return nil, errors.New("spawn broke")
			}
			return fixture.Spawn(selfExe, cwd)
		}
	})
	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "k", Cwd: "/"})
	if r.OK || r.Code != protocol.ErrFixtureStart {
		t.Fatalf("want FIXTURE_START_FAILED, got %+v", r)
	}
	r = do(t, c, protocol.Request{Op: protocol.OpListSessions})
	if len(r.Sessions) != 0 {
		t.Fatalf("registry must be empty: %+v", r.Sessions)
	}
	atomic.StoreInt32(&failSpawn, 0)
	r = do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "k", Cwd: "/"})
	if !r.OK {
		t.Fatalf("retry must succeed: %+v", r)
	}
}

// TestProtocolMismatchPeer: a live peer answering with v != 1 must surface
// ErrProtocolMismatch — never ErrNotRunning — and Ensure must not spawn.
func TestProtocolMismatchPeer(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(root, 0o755)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}

	// Fake peer: accepts, replies with protocol v+1.
	ln, err := net.Listen("unix", p.DaemonSocket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c) // drain request
				c.Write([]byte(fmt.Sprintf(`{"v":%d,"ok":false,"code":"PROTOCOL_MISMATCH","error":"daemon speaks v%d"}`+"\n",
					protocol.Version+1, protocol.Version+1)))
			}(conn)
		}
	}()

	_, err = Dial(p)
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("Dial: want ErrProtocolMismatch, got %v", err)
	}

	var spawned int32
	old := spawnDaemonFunc
	spawnDaemonFunc = func(paths.Paths, string) error {
		atomic.AddInt32(&spawned, 1)
		return nil
	}
	defer func() { spawnDaemonFunc = old }()

	_, err = Ensure(p, "/unused")
	if !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("Ensure: want ErrProtocolMismatch, got %v", err)
	}
	if n := atomic.LoadInt32(&spawned); n != 0 {
		t.Fatalf("Ensure spawned %d daemons against an incompatible peer", n)
	}
}

// TestBadPeer: a live socket that is not a Relay daemon → ErrBadPeer,
// Ensure still does not spawn.
func TestBadPeer(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(root, 0o755)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSocket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c)
				c.Write([]byte("not-json\n"))
			}(conn)
		}
	}()

	_, err = Dial(p)
	if !errors.Is(err, ErrBadPeer) {
		t.Fatalf("Dial: want ErrBadPeer, got %v", err)
	}
	var spawned int32
	old := spawnDaemonFunc
	spawnDaemonFunc = func(paths.Paths, string) error {
		atomic.AddInt32(&spawned, 1)
		return nil
	}
	defer func() { spawnDaemonFunc = old }()
	if _, err := Ensure(p, "/unused"); !errors.Is(err, ErrBadPeer) {
		t.Fatalf("Ensure: want ErrBadPeer, got %v", err)
	}
	if n := atomic.LoadInt32(&spawned); n != 0 {
		t.Fatalf("Ensure spawned %d daemons against a bad peer", n)
	}
}

// TestReadDeadlineEnforced: an idle connection is closed by the server's
// read deadline — proven with a short test timeout, not 5s.
func TestReadDeadlineEnforced(t *testing.T) {
	_, p, _ := startInProcess(t, func(o *Options) {
		o.ReadTimeout = 150 * time.Millisecond
	})
	c, err := net.DialTimeout("unix", p.DaemonSocket(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second)) // generous test bound
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("idle connection was not closed by server read deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle close took %s — read deadline not enforced", elapsed)
	}
}

// TestClientDeadlineSeparation: the client's read deadline is its own, not
// a combined read+write. A server that accepts but never responds must hit
// the client read deadline.
func TestClientDeadlineSeparation(t *testing.T) {
	base := t.TempDir()
	sock := filepath.Join(base, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { // reads the request, never replies
				defer c.Close()
				io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	cl := &Client{sock: sock, readTimeout: 150 * time.Millisecond, writeTimeout: ReadTimeout}
	start := time.Now()
	_, err = cl.Do(protocol.Request{Op: protocol.OpPing})
	if err == nil {
		t.Fatal("silent peer must produce an error")
	}
	if !errors.Is(err, ErrBadPeer) {
		t.Fatalf("want ErrBadPeer, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("client read deadline not enforced: %s", elapsed)
	}
}

// TestKeyValidationAllOps: serve/status/stop reject malformed keys with
// INVALID_SESSION_KEY (not SESSION_NOT_FOUND).
func TestKeyValidationAllOps(t *testing.T) {
	_, _, c := startInProcess(t, nil)
	bad := []string{"", "../x", "bad key", "a/b", ".dot"}
	for _, op := range []string{protocol.OpServeFixture, protocol.OpSessionStatus, protocol.OpStopSession} {
		for _, k := range bad {
			req := protocol.Request{Op: op, Key: k, Cwd: "/"}
			r := do(t, c, req)
			if r.OK || r.Code != protocol.ErrInvalidSessionKey {
				t.Fatalf("%s key=%q: want INVALID_SESSION_KEY, got %+v", op, k, r)
			}
		}
	}
}
