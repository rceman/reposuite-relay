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
	"os/exec"
	"path/filepath"
	"strings"
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
// option overrides. It is Serve'd in a goroutine (result on `served`) and
// Shutdown on cleanup.
func startInProcess(t *testing.T, mutate func(*Options)) (*Daemon, paths.Paths, *Client, chan error) {
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
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	t.Cleanup(d.Shutdown)

	// Wait until the socket answers ping.
	c := newClient(p.DaemonSocket())
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.Ping(); err == nil {
			return d, p, c, served
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-process daemon did not become ready")
	return nil, paths.Paths{}, nil, nil
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
	_, _, c, _ := startInProcess(t, func(o *Options) {
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
	_, _, c, _ := startInProcess(t, func(o *Options) {
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
	_, _, c, _ := startInProcess(t, nil)

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
	_, _, c, _ := startInProcess(t, func(o *Options) {
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
	_, _, c, _ := startInProcess(t, func(o *Options) {
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
	_, p, _, _ := startInProcess(t, func(o *Options) {
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
	_, _, c, _ := startInProcess(t, nil)
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

// ---------------------------------------------------------------------
// DMN03: shutdown quiescence + fixture stop semantics
// ---------------------------------------------------------------------

// procAlive reports whether pid exists and is not a zombie (same-package
// copy; the _test package helper is not visible here).
func procAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func waitProcDead(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !procAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, d)
}

// TestCreateVsShutdown: a create in flight (key reserved, spawn blocked)
// must resolve before the final drain — the committed session is drained
// and its fixture stopped; the registry ends empty.
func TestCreateVsShutdown(t *testing.T) {
	spawnGate := make(chan struct{})
	spawnRelease := make(chan struct{})
	var childPID int32
	var spawns int32

	d, _, c, served := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			atomic.AddInt32(&spawns, 1)
			close(spawnGate) // key reserved, spawn in flight
			<-spawnRelease   // hold the handler inside the lifecycle op
			child, err := fixture.Spawn(selfExe, cwd)
			if err == nil {
				atomic.StoreInt32(&childPID, int32(child.PID()))
			}
			return child, err
		}
	})

	serveDone := make(chan struct{})
	go func() {
		_, _ = c.Do(protocol.Request{Op: protocol.OpServeFixture, Key: "racer", Cwd: "/"})
		close(serveDone)
	}()

	<-spawnGate // handler owns the reservation mid-spawn
	d.Shutdown()

	// Serve must not finish while the create handler is in flight.
	select {
	case <-served:
		t.Fatal("Serve returned while a create handler was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(spawnRelease) // let the create resolve
	<-serveDone         // handler exits (conn already closed — fine)

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return after quiescence")
	}

	if n := atomic.LoadInt32(&spawns); n != 1 {
		t.Fatalf("spawns=%d", n)
	}
	if d.registry.Len() != 0 {
		t.Fatalf("registry len=%d after shutdown", d.registry.Len())
	}
	waitProcDead(t, int(atomic.LoadInt32(&childPID)), 3*time.Second)

	// The registry is closed: no late Commit/Reserve can resurrect it.
	if d.registry.Reserve("late") {
		t.Fatal("Reserve succeeded after drain")
	}
}

// TestStopVsShutdown: a stop in flight (BeginStop owned, Stop blocked) —
// shutdown waits for it; the underlying Stop runs exactly once; no drain
// races the same generation.
func TestStopVsShutdown(t *testing.T) {
	stopEntered := make(chan struct{})
	stopRelease := make(chan struct{})
	var underlyingStops int32

	d, _, c, served := startInProcess(t, func(o *Options) {
		o.WrapStop = func(stop func() error) func() error {
			return func() error {
				close(stopEntered) // BeginStop owner inside Stop
				<-stopRelease      // hold the lifecycle op
				atomic.AddInt32(&underlyingStops, 1)
				return stop()
			}
		}
	})

	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "victim", Cwd: "/"})
	if !r.OK {
		t.Fatalf("serve: %+v", r)
	}

	stopDone := make(chan *protocol.Response, 1)
	go func() {
		r, _ := c.Do(protocol.Request{Op: protocol.OpStopSession, Key: "victim"})
		stopDone <- r
	}()

	<-stopEntered // the request handler owns the stop and is inside it
	d.Shutdown()

	// Shutdown must quiesce — Serve cannot return while Stop is blocked.
	select {
	case <-served:
		t.Fatal("Serve returned while a stop handler was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(stopRelease)
	<-stopDone // stop request resolved (response may be lost on closed conn)

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return")
	}
	if n := atomic.LoadInt32(&underlyingStops); n != 1 {
		t.Fatalf("underlying Stop invoked %d times, want exactly 1", n)
	}
	if d.registry.Len() != 0 {
		t.Fatalf("registry len=%d", d.registry.Len())
	}
}

// TestShutdownOpResponse: the `shutdown` request itself gets its OK
// response written before teardown — not torn down mid-reply.
func TestShutdownOpResponse(t *testing.T) {
	_, _, c, served := startInProcess(t, nil)
	r := do(t, c, protocol.Request{Op: protocol.OpShutdown})
	if !r.OK {
		t.Fatalf("shutdown response: %+v", r)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return after shutdown op")
	}
}

// TestShutdownWithIdleConn: a connected client that sent nothing is closed
// by shutdown — it must not delay shutdown for the read deadline.
func TestShutdownWithIdleConn(t *testing.T) {
	_, p, _, served := startInProcess(t, nil)
	idle, err := net.DialTimeout("unix", p.DaemonSocket(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(30 * time.Millisecond) // let the accept loop register it

	// In-process Shutdown — Serve must return promptly (< read deadline).
	// Use the protocol op to also prove response delivery.
	c := newClient(p.DaemonSocket())
	r := do(t, c, protocol.Request{Op: protocol.OpShutdown})
	if !r.OK {
		t.Fatalf("shutdown: %+v", r)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle connection delayed shutdown")
	}
	// The idle conn was closed server-side.
	idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle conn still open after shutdown")
	}
}

// TestStopSessionForcedKill: a stubborn child (ignores stdin EOF) reaches
// the SIGKILL fallback; confirmed reaping → stop_session succeeds and the
// session is removed — not INTERNAL/AbortStop.
func TestStopSessionForcedKill(t *testing.T) {
	var childPID int32
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (*fixture.Child, error) {
			// /bin/sleep ignores stdin and never exits on EOF — exercises
			// the real forced-kill path through fixture.Child.Stop.
			cmd := exec.Command("sleep", "600")
			cmd.Dir = cwd
			child, err := fixture.SpawnCmd(cmd)
			if err == nil {
				atomic.StoreInt32(&childPID, int32(child.PID()))
			}
			return child, err
		}
	})

	r := do(t, c, protocol.Request{Op: protocol.OpServeFixture, Key: "stubborn", Cwd: "/"})
	if !r.OK {
		t.Fatalf("serve: %+v", r)
	}
	pid := int(atomic.LoadInt32(&childPID))
	if pid == 0 || !procAlive(pid) {
		t.Fatal("stubborn child not running")
	}

	// The child ignores stdin EOF → StopTimeout(3s) → SIGKILL → reaped.
	r = do(t, c, protocol.Request{Op: protocol.OpStopSession, Key: "stubborn"})
	if !r.OK {
		t.Fatalf("stop_session after forced kill must succeed: %+v", r)
	}
	waitProcDead(t, pid, 2*time.Second)

	r = do(t, c, protocol.Request{Op: protocol.OpSessionStatus, Key: "stubborn"})
	if r.OK || r.Code != protocol.ErrSessionNotFound {
		t.Fatalf("status after forced-kill stop: %+v", r)
	}
}
