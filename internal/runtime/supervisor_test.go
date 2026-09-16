package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	rt, err := sup.Ensure(key, kind, id, shared, func() (Harness, error) { return proc, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.MarkReady(key); err != nil {
		t.Fatal(err)
	}
	return rt
}

// TestEnsureConvergesOnOneSpawn: concurrent Ensure for one key spawns
// exactly one process; losers' processes are stopped, never leaked.
func TestEnsureConvergesOnOneSpawn(t *testing.T) {
	sup := New()
	var spawns int32
	const n = 16
	procs := make([]*fakeProc, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		procs[i] = newFakeProc(1000 + i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := procs[i]
			_, err := sup.Ensure("codex/default", "codex", "rt", true, func() (Harness, error) {
				atomic.AddInt32(&spawns, 1)
				time.Sleep(2 * time.Millisecond)
				return p, nil
			})
			if err != nil {
				t.Errorf("ensure: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&spawns); got != n {
		t.Fatalf("spawn calls = %d, want %d (each racer attempts)", got, n)
	}
	if sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1", sup.Len())
	}
	// Every loser's process must have been stopped.
	var stopped int32
	for _, p := range procs {
		stopped += atomic.LoadInt32(&p.stops)
	}
	if stopped != n-1 {
		t.Fatalf("loser stops = %d, want %d", stopped, n-1)
	}
	_ = sup.StopAll()
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
// own, bindings disappear (sessions project COLD) and OnExit fires.
func TestUnexpectedExitUnbindsSessions(t *testing.T) {
	sup := New()
	proc := newFakeProc(7000)
	exited := make(chan string, 1)
	sup.OnExit = func(key string, rt *Runtime) { exited <- key }
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

// TestFailStartReapsWithoutRegistering: a failed handshake stops the
// process tree and leaves no runtime registered.
func TestFailStartReapsWithoutRegistering(t *testing.T) {
	sup := New()
	proc := newFakeProc(8000)
	if _, err := sup.Ensure("codex/default", "codex", "rt-1", true,
		func() (Harness, error) { return proc, nil }); err != nil {
		t.Fatal(err)
	}
	if err := sup.FailStart("codex/default"); err != nil {
		t.Fatal(err)
	}
	if sup.Len() != 0 {
		t.Fatal("failed start must not leave a runtime registered")
	}
	if got := atomic.LoadInt32(&proc.stops); got != 1 {
		t.Fatalf("stops = %d, want 1", got)
	}
}

// TestStopAllStopsEveryRuntime: daemon shutdown stops dedicated and
// shared runtimes alike.
func TestStopAllStopsEveryRuntime(t *testing.T) {
	sup := New()
	a := newFakeProc(9000)
	b := newFakeProc(9001)
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
	// The supervisor is closed: no new runtime may be created.
	if _, err := sup.Ensure("x", "codex", "rt-3", true,
		func() (Harness, error) { return newFakeProc(1), nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ensure after close: want ErrClosed, got %v", err)
	}
}
