package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/session"
)

// fakeProc is a controlled in-memory harness for supervisor tests.
type fakeProc struct {
	pid     int
	stops   int32
	stopErr error
	done    chan struct{}
}

func newFakeProc(pid int) *fakeProc {
	return &fakeProc{pid: pid, done: make(chan struct{})}
}

func (f *fakeProc) PID() int              { return f.pid }
func (f *fakeProc) Wait() <-chan struct{} { return f.done }
func (f *fakeProc) Stop() error {
	atomic.AddInt32(&f.stops, 1)
	f.exit(nil)
	return f.stopErr
}

// exit simulates the process exiting on its own.
func (f *fakeProc) exit(err error) {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
}

func mkRuntime(t *testing.T, sup *Supervisor, key, kind, id string, shared bool, proc *fakeProc) *Runtime {
	t.Helper()
	rt, err := sup.Ensure(context.Background(), key, kind, id, shared,
		func() (Harness, error) { return proc, nil })
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// TestEnsureConvergesOnOneSpawn: 16 concurrent wake-ups on a cold runtime
// spawn exactly ONE process and every caller gets the same runtime. The
// losers wait for the winner's generation instead of starting a second
// process (claim-first creation).
func TestEnsureConvergesOnOneSpawn(t *testing.T) {
	sup := New()
	var spawns int32
	const n = 16
	winner := newFakeProc(1000)
	rts := make([]*Runtime, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rts[i], errs[i] = sup.Ensure(context.Background(), "codex/default", "codex", "rt", true,
				func() (Harness, error) {
					atomic.AddInt32(&spawns, 1)
					time.Sleep(2 * time.Millisecond) // hold the claim open
					return winner, nil
				})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
		if rts[i] != rts[0] {
			t.Fatalf("caller %d got a different runtime generation", i)
		}
	}
	if got := atomic.LoadInt32(&spawns); got != 1 {
		t.Fatalf("spawn calls = %d, want exactly 1", got)
	}
	if sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1", sup.Len())
	}
	if got := atomic.LoadInt32(&winner.stops); got != 0 {
		t.Fatalf("winner stopped %d times, want 0", got)
	}
	if rt := rts[0]; rt.PID != winner.PID() || rt.State != session.RuntimeWarm {
		t.Fatalf("runtime = %+v", rt)
	}
	_ = sup.StopAll()
}

// TestEnsureWaiterRespectsContext: a waiter released by a failed spawn
// returns the spawn error, and a waiter whose context ends returns
// promptly instead of blocking on another caller's spawn.
func TestEnsureWaiterRespectsContext(t *testing.T) {
	sup := New()
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := sup.Ensure(context.Background(), "codex/default", "codex", "rt", true,
			func() (Harness, error) {
				<-release
				return nil, errors.New("boom")
			})
		first <- err
	}()
	// Wait until the claim is visible to other callers.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := sup.Get("codex/default"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claim never appeared")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := sup.Ensure(ctx, "codex/default", "codex", "rt2", true,
		func() (Harness, error) { t.Error("waiter must not spawn"); return nil, nil }); err == nil {
		t.Fatal("waiter with an expired context must fail")
	}
	close(release)
	if err := <-first; err == nil || err.Error() != "boom" {
		t.Fatalf("owner err = %v, want the spawn error", err)
	}
	if sup.Len() != 0 {
		t.Fatal("a failed spawn must leave no runtime registered")
	}
	// The claim is released: a later caller can create the runtime.
	proc := newFakeProc(777)
	if _, err := sup.Ensure(context.Background(), "codex/default", "codex", "rt3", true,
		func() (Harness, error) { return proc, nil }); err != nil {
		t.Fatalf("recreate after failure: %v", err)
	}
	if sup.Len() != 1 {
		t.Fatal("runtime not recreated after a failed claim")
	}
}

// TestStopDuringSpawnAbandonsTheClaim: stopping a runtime whose process is
// still spawning never leaks that process and never registers it.
func TestStopDuringSpawnAbandonsTheClaim(t *testing.T) {
	sup := New()
	proc := newFakeProc(555)
	started := make(chan struct{})
	ensureErr := make(chan error, 1)
	go func() {
		_, err := sup.Ensure(context.Background(), "codex/default", "codex", "rt", true,
			func() (Harness, error) {
				close(started)
				time.Sleep(30 * time.Millisecond) // still spawning
				return proc, nil
			})
		ensureErr <- err
	}()
	<-started
	if err := sup.Stop("codex/default"); err != nil {
		t.Fatalf("stop during spawn: %v", err)
	}
	if err := <-ensureErr; !errors.Is(err, ErrRuntimeGone) {
		t.Fatalf("ensure err = %v, want ErrRuntimeGone", err)
	}
	if got := atomic.LoadInt32(&proc.stops); got != 1 {
		t.Fatalf("abandoned process stops = %d, want 1", got)
	}
	if sup.Len() != 0 {
		t.Fatal("abandoned claim must not be registered")
	}
}

// TestSharedRuntimeSurvivesSessionDelete: five sessions on one shared
// runtime; deleting one detaches it and leaves the runtime (and the other
// four bindings) untouched.
func TestSharedRuntimeSurvivesSessionDelete(t *testing.T) {
	sup := New()
	proc := newFakeProc(4242)
	mkRuntime(t, sup, "codex/default", "codex", "rt-1", true, proc)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		if err := sup.Bind("codex/default", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := sup.StopSession("s3"); err != nil {
		t.Fatal(err)
	}
	if sup.Len() != 1 {
		t.Fatal("shared runtime must survive a single session delete")
	}
	if got := atomic.LoadInt32(&proc.stops); got != 0 {
		t.Fatalf("shared runtime stopped %d times, want 0", got)
	}
	if _, ok := sup.View("s3"); ok {
		t.Fatal("deleted session must be unbound")
	}
	for _, id := range []string{"s1", "s2", "s4", "s5"} {
		if _, ok := sup.View(id); !ok {
			t.Fatalf("session %s lost its runtime binding", id)
		}
	}
	// The other sessions still project the same runtime identity.
	v1, _ := sup.View("s1")
	v2, _ := sup.View("s5")
	if v1.RuntimeID != v2.RuntimeID || v1.PID != v2.PID {
		t.Fatal("shared runtime projection diverged")
	}
}

// TestDedicatedRuntimeStopsOnSessionDelete: a dedicated (fixture) runtime
// is stopped and reaped when its session is deleted.
func TestDedicatedRuntimeStopsOnSessionDelete(t *testing.T) {
	sup := New()
	proc := newFakeProc(5252)
	mkRuntime(t, sup, "fixture/s1", "fixture", "rt-1", false, proc)
	if err := sup.Bind("fixture/s1", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := sup.StopSession("s1"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&proc.stops); got != 1 {
		t.Fatalf("dedicated runtime stops = %d, want 1", got)
	}
	if sup.Len() != 0 {
		t.Fatal("dedicated runtime must be gone")
	}
	if _, ok := sup.View("s1"); ok {
		t.Fatal("session must project COLD")
	}
	// Stopping a COLD session is a no-op.
	if err := sup.StopSession("s1"); err != nil {
		t.Fatal(err)
	}
}

// TestSleepBlockers: active turns, pending input, and in-flight mutations
// all block sleep; CanSleep/StopIfIdle agree, and the runtime survives a
// refused stop.
func TestSleepBlockers(t *testing.T) {
	sup := New()
	proc := newFakeProc(6000)
	mkRuntime(t, sup, "codex/default", "codex", "rt-1", true, proc)
	if err := sup.Bind("codex/default", "s1"); err != nil {
		t.Fatal(err)
	}

	if ok, why := sup.CanSleep("codex/default"); !ok {
		t.Fatalf("idle runtime must be sleepable: %s", why)
	}
	sup.SetActivity("s1", ActivityActive)
	if ok, why := sup.CanSleep("codex/default"); ok || why == "" {
		t.Fatal("active turn must block sleep")
	}
	if err := sup.StopIfIdle("codex/default"); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle with active turn: want ErrRuntimeBusy, got %v", err)
	}
	if sup.Len() != 1 || atomic.LoadInt32(&proc.stops) != 0 {
		t.Fatal("refused stop must not kill the runtime")
	}

	sup.SetActivity("s1", ActivityWaitingInput)
	if err := sup.StopIfIdle("codex/default"); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle with pending input: want ErrRuntimeBusy, got %v", err)
	}
	if sup.Activity("s1") != ActivityWaitingInput {
		t.Fatalf("activity = %s", sup.Activity("s1"))
	}

	sup.SetActivity("s1", ActivityIdle)
	if err := sup.BeginMutation("s1"); err != nil {
		t.Fatal(err)
	}
	if err := sup.StopIfIdle("codex/default"); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("StopIfIdle with mutation: want ErrRuntimeBusy, got %v", err)
	}
	// A second concurrent mutation is refused (serialization).
	if err := sup.BeginMutation("s1"); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("overlapping mutation: want ErrRuntimeBusy, got %v", err)
	}
	sup.EndMutation("s1")

	// Now the runtime may sleep: StopIfIdle succeeds and the process is
	// reaped.
	if err := sup.StopIfIdle("codex/default"); err != nil {
		t.Fatalf("StopIfIdle: %v", err)
	}
	if got := atomic.LoadInt32(&proc.stops); got != 1 {
		t.Fatalf("stops = %d, want 1", got)
	}
	if sup.Len() != 0 {
		t.Fatal("runtime must be gone after sleep")
	}
	if _, ok := sup.View("s1"); ok {
		t.Fatal("session must project COLD after runtime sleep")
	}
	if err := sup.StopIfIdle("codex/default"); !errors.Is(err, ErrRuntimeGone) {
		t.Fatalf("StopIfIdle on absent runtime: want ErrRuntimeGone, got %v", err)
	}
}

// TestUnexpectedExitUnbindsSessions: when a runtime process dies on its
// own, bindings disappear (sessions project COLD) and OnGone fires with
// the exit reason.
func TestUnexpectedExitUnbindsSessions(t *testing.T) {
	sup := New()
	proc := newFakeProc(7000)
	exited := make(chan string, 1)
	sup.OnGone = func(key string, rt *Runtime, reason string) {
		if reason != ReasonExited {
			t.Errorf("reason = %q", reason)
		}
		exited <- key
	}
	mkRuntime(t, sup, "codex/default", "codex", "rt-1", true, proc)
	for _, id := range []string{"s1", "s2"} {
		if err := sup.Bind("codex/default", id); err != nil {
			t.Fatal(err)
		}
	}
	proc.exit(errors.New("boom"))
	select {
	case key := <-exited:
		if key != "codex/default" {
			t.Fatalf("OnExit key = %s", key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnExit not called")
	}
	for _, id := range []string{"s1", "s2"} {
		if _, ok := sup.View(id); ok {
			t.Fatalf("session %s still bound after runtime death", id)
		}
	}
	if sup.Len() != 0 {
		t.Fatal("dead runtime must be removed")
	}
}

// TestSpawnFailureReapsAndRegistersNothing: the spawn closure owns its
// process; a failed handshake stops it and leaves no runtime registered.
func TestSpawnFailureReapsAndRegistersNothing(t *testing.T) {
	sup := New()
	proc := newFakeProc(8000)
	_, err := sup.Ensure(context.Background(), "codex/default", "codex", "rt-1", true,
		func() (Harness, error) {
			_ = proc.Stop() // the closure reaps what it started
			return nil, errors.New("initialize failed")
		})
	if err == nil {
		t.Fatal("failed spawn must return an error")
	}
	if sup.Len() != 0 {
		t.Fatal("failed start must not leave a runtime registered")
	}
	if got := atomic.LoadInt32(&proc.stops); got != 1 {
		t.Fatalf("stops = %d, want 1", got)
	}
}

// TestStopAllStopsEveryRuntime: daemon shutdown stops dedicated and
// shared runtimes alike, and reports each one gone as a deliberate stop.
func TestStopAllStopsEveryRuntime(t *testing.T) {
	sup := New()
	a := newFakeProc(9000)
	b := newFakeProc(9001)
	var mu sync.Mutex
	reasons := map[string]string{}
	sup.OnGone = func(key string, rt *Runtime, reason string) {
		mu.Lock()
		reasons[key] = reason
		mu.Unlock()
	}
	mkRuntime(t, sup, "fixture/s1", "fixture", "rt-1", false, a)
	mkRuntime(t, sup, "codex/default", "codex", "rt-2", true, b)
	if err := sup.StopAll(); err != nil {
		t.Fatal(err)
	}
	if sup.Len() != 0 {
		t.Fatal("runtimes remain after StopAll")
	}
	if atomic.LoadInt32(&a.stops) != 1 || atomic.LoadInt32(&b.stops) != 1 {
		t.Fatal("not every runtime was stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 2 || reasons["fixture/s1"] != ReasonStopped || reasons["codex/default"] != ReasonStopped {
		t.Fatalf("OnGone reasons = %v", reasons)
	}
	// The supervisor is closed: no new runtime may be created.
	if _, err := sup.Ensure(context.Background(), "x", "codex", "rt-3", true,
		func() (Harness, error) { return newFakeProc(1), nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ensure after close: want ErrClosed, got %v", err)
	}
}
