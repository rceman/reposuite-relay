package daemon

// Same-package tests for the lifecycle/control-plane corrections: they run
// the daemon in-process (Start/Serve/Shutdown) so test seams — spawn
// counting, randomness failure, short deadlines, spawn suppression — are
// usable without a second binary. All client traffic goes through the
// production internal/client package over loopback HTTP.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// testBinary returns the production binary built by TestMain (published
// through REPOSUITE_TEST_BIN), which implements the `__fixture` child mode.
func testBinary() string {
	p := os.Getenv("REPOSUITE_TEST_BIN")
	if p == "" {
		panic("REPOSUITE_TEST_BIN not set — TestMain must build the production binary first")
	}
	return p
}

// startInProcess brings up a daemon on an isolated root with the given
// option overrides. It is Serve'd in a goroutine (result on `served`) and
// Shutdown on cleanup.
func startInProcess(t *testing.T, mutate func(*Options)) (*Daemon, paths.Paths, *client.Client, chan error) {
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
	// The production binary implements `__fixture`, so the default spawn
	// path is the real one; tests that need controlled/stubborn children
	// override SpawnFixture. The binary is built once by TestMain in the
	// external test package.
	opts := Options{SelfExe: testBinary()}
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

	// Wait until the daemon answers an authenticated ping.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return d, p, c, served
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-process daemon did not become ready")
	return nil, paths.Paths{}, nil, nil
}

// tctx returns a bounded context for one client call.
func tctx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// serve creates a fixture session; the error is returned as-is so tests
// can assert stable API codes.
func serve(t *testing.T, c *client.Client, key string) (api.SessionInfo, error) {
	t.Helper()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.ServeFixture(ctx, key, "/")
	return resp.Session, err
}

// wantAPIErr asserts err is an APIError with the given code.
func wantAPIErr(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want API error %s, got nil", code)
	}
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want API error %s, got %v", code, err)
	}
	if ae.Code != code {
		t.Fatalf("want API error %s, got %s (%v)", code, ae.Code, err)
	}
}

// TestDuplicateKeyNoSpawn: a duplicate serve must create ZERO extra
// fixture processes — the key is reserved before spawn.
func TestDuplicateKeyNoSpawn(t *testing.T) {
	var spawns int32
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
			atomic.AddInt32(&spawns, 1)
			return fixture.Spawn(selfExe, cwd)
		}
	})

	if _, err := serve(t, c, "same"); err != nil {
		t.Fatalf("first serve failed: %v", err)
	}
	_, err := serve(t, c, "same")
	wantAPIErr(t, err, api.ErrSessionExists)
	if n := atomic.LoadInt32(&spawns); n != 1 {
		t.Fatalf("spawn count = %d, want 1 — duplicate spawned a process", n)
	}
}

// TestSameKeyCreateRace: 16 concurrent serves for the SAME key — exactly
// one succeeds, one fixture is spawned, one session exists.
func TestSameKeyCreateRace(t *testing.T) {
	var spawns int32
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := c.ServeFixture(ctx, "raced", "/")
			if err == nil {
				codes[i] = "OK"
				return
			}
			var ae *client.APIError
			if errors.As(err, &ae) {
				codes[i] = ae.Code
				return
			}
			codes[i] = "TRANSPORT:" + err.Error()
		}(i)
	}
	wg.Wait()

	okCount, exists := 0, 0
	for _, code := range codes {
		switch code {
		case "OK":
			okCount++
		case api.ErrSessionExists:
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
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Status(ctx, "raced"); err != nil {
		t.Fatalf("final session missing: %v", err)
	}
}

// TestSameKeyStopRace: concurrent stops — exactly one caller owns the stop
// (BeginStop exclusivity), session removed; losers get SESSION_NOT_FOUND.
// Single-invocation of the generation's Stop is proven separately at the
// registry level in TestStopOwnershipSingleInvocation.
func TestSameKeyStopRace(t *testing.T) {
	_, _, c, _ := startInProcess(t, nil)

	if _, err := serve(t, c, "victim"); err != nil {
		t.Fatalf("serve: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := c.StopSession(ctx, "victim")
			if err == nil {
				codes[i] = "OK"
				return
			}
			var ae *client.APIError
			if errors.As(err, &ae) {
				codes[i] = ae.Code
				return
			}
			codes[i] = "TRANSPORT:" + err.Error()
		}(i)
	}
	wg.Wait()

	okCount, notFound := 0, 0
	for _, code := range codes {
		switch code {
		case "OK":
			okCount++
		case api.ErrSessionNotFound:
			notFound++
		default:
			t.Fatalf("unexpected %q", code)
		}
	}
	if okCount != 1 || notFound != n-1 {
		t.Fatalf("ok=%d notFound=%d (want 1/%d)", okCount, notFound, n-1)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Status(ctx, "victim"); err == nil {
		t.Fatal("session must be gone")
	}
}

// fakeHarness is a controlled in-memory runtime process for supervisor
// tests: no OS process, deterministic stop accounting.
type fakeHarness struct {
	pid     int
	stopped int32
	stopErr error
	done    chan struct{}
}

func newFakeHarness(pid int) *fakeHarness {
	return &fakeHarness{pid: pid, done: make(chan struct{})}
}

func (f *fakeHarness) PID() int              { return f.pid }
func (f *fakeHarness) Wait() <-chan struct{} { return f.done }
func (f *fakeHarness) Stop() error {
	atomic.AddInt32(&f.stopped, 1)
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return f.stopErr
}

// TestStopOwnershipSingleInvocation — supervisor-level proof that
// concurrent session deletion stops a dedicated runtime exactly once.
func TestStopOwnershipSingleInvocation(t *testing.T) {
	sup := runtime.New()
	fake := newFakeHarness(4242)
	rt, err := sup.Ensure("fixture/abc", session.HarnessFixture, "x", false,
		func() (runtime.Harness, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.MarkReady(rt.Key); err != nil {
		t.Fatal(err)
	}
	if err := sup.Bind(rt.Key, "abc"); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sup.StopSession("abc")
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&fake.stopped); got != 1 {
		t.Fatalf("runtime Stop invoked %d times, want 1", got)
	}
	if sup.Len() != 0 {
		t.Fatal("runtime must be gone")
	}
	if _, ok := sup.View("abc"); ok {
		t.Fatal("session must project COLD after runtime stop")
	}
}

// TestRuntimeIDFailure: crypto/rand failure → clean structured failure,
// zero spawns, empty registry, key retryable.
func TestRuntimeIDFailure(t *testing.T) {
	var spawns int32
	failRand := errors.New("no entropy")
	var randFails int32 = 1
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
			atomic.AddInt32(&spawns, 1)
			return fixture.Spawn(selfExe, cwd)
		}
		o.RandSessionID = func() (string, error) {
			if atomic.LoadInt32(&randFails) == 1 {
				return "", failRand
			}
			return session.NewSessionID()
		}
	})

	_, err := serve(t, c, "k")
	wantAPIErr(t, err, api.ErrInternal)
	if n := atomic.LoadInt32(&spawns); n != 0 {
		t.Fatalf("spawn count = %d, want 0 on rand failure", n)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Status(ctx, "k"); err == nil {
		t.Fatal("session must not exist")
	}
	// Retry with working randomness succeeds — reservation was released.
	atomic.StoreInt32(&randFails, 0)
	if _, err := serve(t, c, "k"); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
}

// TestSpawnFailure: fixture spawn error → INTERNAL, empty registry,
// reservation released, retry works.
func TestSpawnFailure(t *testing.T) {
	var failSpawn int32 = 1
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
			if atomic.LoadInt32(&failSpawn) == 1 {
				return nil, errors.New("spawn broke")
			}
			return fixture.Spawn(selfExe, cwd)
		}
	})
	_, err := serve(t, c, "k")
	wantAPIErr(t, err, api.ErrInternal)
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.Sessions(ctx)
	if err != nil || len(resp.Sessions) != 0 {
		t.Fatalf("registry must be empty: %v %v", resp.Sessions, err)
	}
	atomic.StoreInt32(&failSpawn, 0)
	if _, err := serve(t, c, "k"); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
}

// TestReadDeadlineEnforced: an idle connection is closed by the server's
// header read deadline — proven with a short test timeout, not 5s.
func TestReadDeadlineEnforced(t *testing.T) {
	_, p, _, _ := startInProcess(t, func(o *Options) {
		o.ReadTimeout = 150 * time.Millisecond
	})
	d, err := readDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(d.Endpoint, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second)) // generous test bound
	start := time.Now()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection was not closed by server read deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle close took %s — read deadline not enforced", elapsed)
	}
}

// TestKeyValidation: bad keys never reach session lookup.
func TestKeyValidation(t *testing.T) {
	_, _, c, _ := startInProcess(t, nil)
	// Body-carried keys (serve): every malformed key is INVALID_SESSION_KEY.
	for _, k := range []string{"", "../x", "bad key", "a/b", ".dot"} {
		_, err := serve(t, c, k)
		wantAPIErr(t, err, api.ErrInvalidSessionKey)
	}
	// Path-carried keys (status/stop): single-segment malformed keys are
	// INVALID_SESSION_KEY; keys containing separators are not routes.
	ctx, cancel := tctx(t)
	defer cancel()
	for _, k := range []string{"bad key", ".dot"} {
		if _, err := c.Status(ctx, k); err == nil {
			t.Fatalf("status key %q accepted", k)
		} else {
			wantAPIErr(t, err, api.ErrInvalidSessionKey)
		}
		if _, err := c.StopSession(ctx, k); err == nil {
			t.Fatalf("stop key %q accepted", k)
		} else {
			wantAPIErr(t, err, api.ErrInvalidSessionKey)
		}
	}
	for _, k := range []string{"", "../x", "a/b"} {
		if _, err := c.Status(ctx, k); err == nil {
			t.Fatalf("status key %q accepted", k)
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
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
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
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = c.ServeFixture(ctx, "racer", "/")
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
	<-serveDone         // handler exits (response may be lost — fine)

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

	if _, err := serve(t, c, "victim"); err != nil {
		t.Fatalf("serve: %v", err)
	}

	stopDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = c.StopSession(ctx, "victim")
		close(stopDone)
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
	<-stopDone // stop request resolved

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

// TestShutdownOpResponse: the shutdown request itself gets its response
// written before teardown — not torn down mid-reply.
func TestShutdownOpResponse(t *testing.T) {
	_, _, c, served := startInProcess(t, nil)
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown response: %v", err)
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
	d, err := readDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := net.DialTimeout("tcp", strings.TrimPrefix(d.Endpoint, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(30 * time.Millisecond) // let the server register it

	c, err := client.Dial(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
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
// the SIGKILL fallback; confirmed reaping → stop succeeds and the session
// is removed — not INTERNAL/AbortStop.
func TestStopSessionForcedKill(t *testing.T) {
	var childPID int32
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
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

	if _, err := serve(t, c, "stubborn"); err != nil {
		t.Fatalf("serve: %v", err)
	}
	pid := int(atomic.LoadInt32(&childPID))
	if pid == 0 || !procAlive(pid) {
		t.Fatal("stubborn child not running")
	}

	// The child ignores stdin EOF → StopTimeout(3s) → SIGKILL → reaped.
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.StopSession(ctx, "stubborn"); err != nil {
		t.Fatalf("stop after forced kill must succeed: %v", err)
	}
	waitProcDead(t, pid, 2*time.Second)

	if _, err := c.Status(ctx, "stubborn"); err == nil {
		t.Fatal("session must be gone after forced-kill stop")
	}
}
