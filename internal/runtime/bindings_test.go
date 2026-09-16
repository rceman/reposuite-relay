package runtime

import (
	"errors"
	"sync/atomic"
	"testing"
)

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
