package daemon

import (
	"context"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"net"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
