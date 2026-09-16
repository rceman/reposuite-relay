package daemon

import (
	"context"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

// TestStopOwnershipSingleInvocation — supervisor-level proof that
// concurrent session deletion stops a dedicated runtime exactly once.
func TestStopOwnershipSingleInvocation(t *testing.T) {
	sup := runtime.New()
	fake := newFakeHarness(4242)
	rt, err := sup.Ensure(context.Background(), "fixture/abc", session.HarnessFixture, "x", false,
		func() (runtime.Harness, error) { return fake, nil })
	if err != nil {
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
