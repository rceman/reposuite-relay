// Orphan-cleanup contract for the shared OpenCode runtime: a failed
// attach reaps only a generation with zero bound sessions, serialized
// attaches make pre-bind kill impossible, and concurrent failures reap
// exactly once per generation.

package opencode

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestFailedLoadKeepsBoundSharedSession: B's failed attach cleanup must
// decline while A is bound to the shared generation (§11).
func TestFailedLoadKeepsBoundSharedSession(t *testing.T) {
	e, a := newEnv(t, acp.FakeLoadError)
	// A binds via session/new — load-error affects only session/load.
	ma := newSession(t, e, a, "bound", t.TempDir())
	if _, err := a.Prompt(bg(), ma, text("bind A")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(ma.Session.ID, api.EventMessageAgentCompleted)
	if e.Supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1", e.Supervisor.Len())
	}

	mb := newSession(t, e, a, "lost", t.TempDir())
	mb.MetaMu.Lock()
	mb.Session.NativeSessionID = "ses_missing"
	mb.MetaMu.Unlock()
	a.Track(mb)
	if _, err := a.Prompt(bg(), mb, text("bind B")); !isNativeSessionLost(err) {
		t.Fatalf("err = %v, want NATIVE_SESSION_LOST", err)
	}

	if e.Supervisor.Len() != 1 {
		t.Fatalf("shared generation killed: runtimes = %d", e.Supervisor.Len())
	}
	if !a.State(ma.Session.ID).Live {
		t.Fatal("bound session A lost its live runtime")
	}
	if e.DurablePayload(ma.Session.ID, api.EventRuntimeExited) != nil {
		t.Fatal("runtime.exited recorded for A's still-live generation")
	}
	if _, err := a.Prompt(bg(), ma, text("still usable")); err != nil {
		t.Fatalf("bound session cannot prompt after foreign attach failure: %v", err)
	}
	harnessenv.WaitFor(t, "A completes second turn", func() bool {
		return len(e.DurablePayloads(ma.Session.ID, api.EventMessageAgentCompleted)) == 2
	})
}

// TestOrphanCleanupCannotKillPreBindAttach: while A holds the shared
// generation inside the serialized attach (pre-bind), B cannot run
// orphan cleanup (§12 TOCTOU proof, no sleeps).
func TestOrphanCleanupCannotKillPreBindAttach(t *testing.T) {
	e := harnessenv.New(t)
	parked := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	rtSeq := 0
	a := NewAdapter(Deps{
		Broker:      e.Broker,
		Supervisor:  e.Supervisor,
		Command:     harnessenv.FakeACPCommand(t, acp.FakeVendorOpenCode, acp.FakeLoadError, e.ACPState),
		Materialize: e.Materialize,
		RandRuntimeID: func() (string, error) {
			rtSeq++
			return fmt.Sprintf("rt-gate-%d", rtSeq), nil
		},
		Version: "test",
		AttachGate: func() {
			if calls.Add(1) == 1 {
				close(parked)
				<-release
			}
		},
	})
	e.Supervisor.OnGone = a.OnRuntimeGone
	e.Drain = a.WaitQuiescent

	ma := newSession(t, e, a, "parked", t.TempDir())
	mb := newSession(t, e, a, "failer", t.TempDir())
	mb.MetaMu.Lock()
	mb.Session.NativeSessionID = "ses_missing"
	mb.MetaMu.Unlock()
	a.Track(mb)

	aDone := make(chan error, 1)
	go func() { _, err := a.Prompt(bg(), ma, text("A")); aDone <- err }()
	<-parked // A is inside the serialized attach, pre-bind.

	bDone := make(chan error, 1)
	go func() { _, err := a.Prompt(bg(), mb, text("B")); bDone <- err }()
	// B is blocked at attachMu — it cannot reach cleanup while A is
	// parked pre-bind. bDone can only carry a result after A releases.
	select {
	case err := <-bDone:
		t.Fatalf("second attach ran during first attach's pre-bind work: %v", err)
	default:
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A failed: %v", err)
	}
	e.WaitDurable(ma.Session.ID, api.EventMessageAgentCompleted)
	if err := <-bDone; !isNativeSessionLost(err) {
		t.Fatalf("B err = %v, want NATIVE_SESSION_LOST", err)
	}
	if e.Supervisor.Len() != 1 {
		t.Fatalf("bound generation killed: runtimes = %d", e.Supervisor.Len())
	}
	if !a.State(ma.Session.ID).Live {
		t.Fatal("A lost its runtime to foreign orphan cleanup")
	}
}

// TestConcurrentFailedAttachesReapOncePerGeneration: serialized failing
// attaches each spawn and reap exactly one generation — no leak, no
// double-stop (§13/§14).
func TestConcurrentFailedAttachesReapOncePerGeneration(t *testing.T) {
	e := harnessenv.New(t)
	var gone atomic.Int64
	rtSeq := 0
	a := NewAdapter(Deps{
		Broker:      e.Broker,
		Supervisor:  e.Supervisor,
		Command:     harnessenv.FakeACPCommand(t, acp.FakeVendorOpenCode, acp.FakeLoadError, e.ACPState),
		Materialize: e.Materialize,
		RandRuntimeID: func() (string, error) {
			rtSeq++
			return fmt.Sprintf("rt-conc-%d", rtSeq), nil
		},
		Version: "test",
	})
	e.Supervisor.OnGone = func(key string, rt *runtime.Runtime, reason string) {
		gone.Add(1)
		a.OnRuntimeGone(key, rt, reason)
	}
	e.Drain = a.WaitQuiescent

	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		m := newSession(t, e, a, fmt.Sprintf("f%d", i), t.TempDir())
		m.MetaMu.Lock()
		m.Session.NativeSessionID = "ses_missing"
		m.MetaMu.Unlock()
		a.Track(m)
		wg.Add(1)
		go func(m *session.Managed) {
			defer wg.Done()
			_, err := a.Prompt(bg(), m, text("x"))
			errs <- err
		}(m)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !isNativeSessionLost(err) && !errors.Is(err, harness.ErrRuntimeUnavailable) {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	harnessenv.WaitFor(t, "orphan generations reaped", func() bool { return e.Supervisor.Len() == 0 })
	e.Supervisor.WaitWatchers()
	if g := gone.Load(); g != n {
		t.Fatalf("OnRuntimeGone calls = %d, want %d (once per generation)", g, n)
	}
}
