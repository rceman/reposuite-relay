package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

func TestMain(m *testing.M) { os.Exit(harnessenv.Main(m)) }

// newEnv builds a real adapter over the deterministic fake ACP agent.
func newEnv(t *testing.T, mode string) (*harnessenv.Env, *Adapter) {
	t.Helper()
	e := harnessenv.New(t)
	// Every generation gets a distinct ephemeral id — Runtime.Key is stable,
	// Runtime.ID is not.
	rtSeq := 0
	a := NewAdapter(Deps{
		Broker:      e.Broker,
		Supervisor:  e.Supervisor,
		Command:     harnessenv.FakeACPCommand(t, acp.FakeVendorOpenCode, mode, e.ACPState),
		Materialize: e.Materialize,
		RandRuntimeID: func() (string, error) {
			rtSeq++
			return fmt.Sprintf("rt-opencode-%d", rtSeq), nil
		},
		Version: "test",
	})
	e.Supervisor.OnGone = a.OnRuntimeGone
	return e, a
}

// newSession creates one COLD OpenCode session (generation 0, no runtime).
func newSession(t *testing.T, e *harnessenv.Env, a *Adapter, key, cwd string) *session.Managed {
	t.Helper()
	return e.NewSession(key, session.HarnessOpenCode, cwd, 0, a.Track)
}

// eventTypes decodes the durable transcript types for readable assertions.
func assertTypes(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("durable types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("durable types = %v, want %v", got, want)
		}
	}
}

// TestCreateIsColdAndRuntimeFree: creation allocates identity only — no
// process, no native session, generation 0.
func TestCreateIsColdAndRuntimeFree(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "cold", t.TempDir())
	snap := m.Snapshot()
	if snap.Harness != session.HarnessOpenCode || snap.NativeSessionID != "" || snap.Generation != 0 {
		t.Fatalf("cold session = %+v", snap)
	}
	if v, ok := e.Supervisor.View(m.Session.ID); ok {
		t.Fatalf("cold session has runtime state %+v", v)
	}
	if ids := acp.FakeSessionIDs(e.ACPState); len(ids) != 0 {
		t.Fatalf("cold session created native sessions: %v", ids)
	}
}

// TestFirstPromptCreatesNativeSessionAndMaterializes: session/new supplies a
// durable `ses_*` identity, which is persisted immediately, and the turn
// completes with canonical events.
func TestFirstPromptCreatesNativeSessionAndMaterializes(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "first", t.TempDir())
	res, err := a.Prompt(bg(), m, text("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeSessionID == "" || res.RuntimeID == "" || res.RuntimeID == RuntimeKey || res.TurnID == "" {
		t.Fatalf("prompt result = %+v", res)
	}
	// The prompt result must carry the actual runtime generation ID, never
	// the stable supervisor key.
	if v, ok := e.Supervisor.View(m.Session.ID); !ok || v.RuntimeID != res.RuntimeID {
		t.Fatalf("runtimeId %q does not match the bound generation %+v", res.RuntimeID, v)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)

	got := e.Reload(m.Session.ID)
	if got.NativeSessionID != res.NativeSessionID {
		t.Fatalf("nativeSessionId = %q, want %q", got.NativeSessionID, res.NativeSessionID)
	}
	// Generation 0 (create) → 1 (first native bind).
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	assertTypes(t, e.DurableTypes(m.Session.ID), []string{
		api.EventMessageUser,
		api.EventNativeSession,
		api.EventHarnessStarted,
		api.EventMessageAgentCompleted,
	})
	var completed api.MessageCompletedPayload
	if err := json.Unmarshal(e.DurablePayload(m.Session.ID, api.EventMessageAgentCompleted), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Text != "echo: hello" {
		t.Fatalf("completed text = %q", completed.Text)
	}
	if st := a.State(m.Session.ID); !st.Materialized || st.TurnInFlight {
		t.Fatalf("adapter state = %+v", st)
	}
}

// TestExactColdResumeUsesSessionLoad: after the runtime generation dies, the
// next prompt reattaches to the EXACT native session and never creates a new
// one.
func TestExactColdResumeUsesSessionLoad(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "resume", t.TempDir())
	first, err := a.Prompt(bg(), m, text("first"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if err := e.Supervisor.Stop(RuntimeKey); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "session COLD after runtime stop", func() bool {
		return !a.State(m.Session.ID).Live
	})

	second, err := a.Prompt(bg(), m, text("second"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if second.NativeSessionID != first.NativeSessionID {
		t.Fatalf("resumed native session = %q, want the exact %q",
			second.NativeSessionID, first.NativeSessionID)
	}
	if got := e.Reload(m.Session.ID); got.Generation != 2 {
		t.Fatalf("generation = %d, want 2 (bind + exact resume)", got.Generation)
	}
	// The resumed generation must have loaded, never created.
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != 1 {
		t.Fatalf("session/load calls = %d, want 1", n)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (no substitution)", n)
	}
	started := e.DurablePayloads(m.Session.ID, api.EventHarnessStarted)
	if len(started) != 2 {
		t.Fatalf("harness.started records = %d", len(started))
	}
	var resumed api.HarnessStartedPayload
	if err := json.Unmarshal(started[1], &resumed); err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed {
		t.Fatal("second generation must report a resume")
	}
}

// TestZeroTurnColdResumeIsExact: OpenCode can reattach to a session with no
// completed turn, so a COLD session with a persisted identity resumes exactly.
func TestZeroTurnColdResumeIsExact(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "zeroturn", t.TempDir())

	// Materialize a native session without completing a turn.
	srv, err := a.ensureRuntime(bg(), m.Snapshot().Cwd)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := acp.OpenSession(bg(), srv, m.Snapshot().Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.recordNative(m, a.state(m.Session.ID), handle.ID(), false); err != nil {
		t.Fatal(err)
	}
	handle.Close()
	if err := e.Supervisor.Stop(RuntimeKey); err != nil {
		t.Fatal(err)
	}
	nativeID := e.Reload(m.Session.ID).NativeSessionID
	if nativeID == "" {
		t.Fatal("no persisted native identity")
	}
	res, err := a.Prompt(bg(), m, text("after zero-turn cold"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if res.NativeSessionID != nativeID {
		t.Fatalf("resumed %q, want the exact %q", res.NativeSessionID, nativeID)
	}
}

// TestFailedExactLoadNeverSubstitutes: a native load failure is a hard failure
// with a stable error and NO fallback session.
func TestFailedExactLoadNeverSubstitutes(t *testing.T) {
	e, a := newEnv(t, acp.FakeLoadError)
	m := newSession(t, e, a, "lost", t.TempDir())
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "ses_99999999"
	m.MetaMu.Unlock()
	a.Track(m) // the adapter mirrors durable identity at Track/restore

	if _, err := a.Prompt(bg(), m, text("hello")); err == nil {
		t.Fatal("exact load failure must fail the prompt")
	} else if !isNativeSessionLost(err) {
		t.Fatalf("err = %v, want NATIVE_SESSION_LOST", err)
	}
	// No new session was created, and the failure is durable.
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 0 {
		t.Fatalf("session/new calls = %d, want 0", n)
	}
	if e.DurablePayload(m.Session.ID, api.EventTurnFailed) == nil {
		t.Fatal("no durable turn.failed record")
	}
	if got := e.Reload(m.Session.ID).NativeSessionID; got != "ses_99999999" {
		t.Fatalf("nativeSessionId = %q, want the unchanged exact identity", got)
	}
}

// TestMalformedNativeIdentityFailsClosed: a malformed durable identity never
// reaches the runtime at all.
func TestMalformedNativeIdentityFailsClosed(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "malformed", t.TempDir())
	m.MetaMu.Lock()
	m.Session.NativeSessionID = "../../etc/passwd"
	m.MetaMu.Unlock()
	a.Track(m)
	if _, err := a.Prompt(bg(), m, text("hello")); !isNativeSessionLost(err) {
		t.Fatalf("err = %v, want NATIVE_SESSION_LOST", err)
	}
	if n := acp.CountFakeEvents(e.ACPState, "initialize"); n != 0 {
		t.Fatalf("a malformed identity must not wake a runtime (initialize = %d)", n)
	}
}

// TestRuntimeDoesNotAdvertiseLoadFailsClosed: a runtime without the
// loadSession capability is not usable for Relay's contract.
func TestRuntimeDoesNotAdvertiseLoadFailsClosed(t *testing.T) {
	e, a := newEnv(t, acp.FakeNoLoad)
	m := newSession(t, e, a, "noload", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hello")); err == nil {
		t.Fatal("a runtime without loadSession must fail closed")
	} else if !isUnsupported(err) {
		t.Fatalf("err = %v, want UNSUPPORTED_OPERATION", err)
	}
}

// turnSettled reports whether a turn is fully done: no in-flight turn AND the
// supervisor sees the session idle. The stronger condition matters for
// mutations — clearing the in-flight marker happens before activity resets.
func turnSettled(e *harnessenv.Env, a *Adapter, sessionID string) bool {
	return !a.State(sessionID).TurnInFlight &&
		e.Supervisor.Activity(sessionID) == runtime.ActivityIdle
}
