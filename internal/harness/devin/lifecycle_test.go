package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/harnessenv"
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
		Broker:     e.Broker,
		Supervisor: e.Supervisor,
		Command: func(model string) (acp.Command, error) {
			return harnessenv.FakeACPCommand(t, acp.FakeVendorDevin, mode, e.ACPState)()
		},
		Materialize: e.Materialize,
		RandRuntimeID: func() (string, error) {
			rtSeq++
			return fmt.Sprintf("rt-devin-%d", rtSeq), nil
		},
		Version: "test",
	})
	e.Supervisor.OnGone = a.OnRuntimeGone
	e.Drain = a.WaitQuiescent
	return e, a
}

func newSession(t *testing.T, e *harnessenv.Env, a *Adapter, key, cwd string) *session.Managed {
	t.Helper()
	return e.NewSession(key, session.HarnessDevin, cwd, 0, a.Track)
}

func bg() context.Context { return context.Background() }

func text(s string) harness.PromptCommand { return harness.PromptCommand{Text: s} }

func isBusy(err error) bool { return errors.Is(err, harness.ErrBusy) }

func isNativeSessionLost(err error) bool { return errors.Is(err, harness.ErrNativeSessionLost) }

func isUnsupported(err error) bool { return errors.Is(err, harness.ErrUnsupported) }

func isInvalidConfig(err error) bool { return errors.Is(err, harness.ErrInvalidConfig) }

func isNoActiveTurn(err error) bool { return errors.Is(err, harness.ErrNoActiveTurn) }

// TestRuntimeKeyPartitionedByModel: Devin's model is a PROCESS-level choice, so
// different models get different runtime keys and the same model shares one.
func TestRuntimeKeyPartitionedByModel(t *testing.T) {
	if RuntimeKeyFor("") != RuntimeKeyFor("   ") {
		t.Fatal("empty and blank models must share the default key")
	}
	if RuntimeKeyFor("") == RuntimeKeyFor("opus") {
		t.Fatal("a model must partition the runtime key")
	}
	if RuntimeKeyFor("claude-4-5-sonnet") != RuntimeKeyFor("claude-4-5-sonnet") {
		t.Fatal("the same model must map to the same key")
	}
	if RuntimeKeyFor("../../etc/passwd") == RuntimeKeyFor("") {
		t.Fatal("an unsafe model must not collapse into the default key")
	}
	if got := RuntimeKeyFor("a/b c"); got != RuntimeKeyPrefix+"a-b-c" {
		t.Fatalf("key = %q", got)
	}
}

// TestCreateIsColdAndRuntimeFree: creation allocates identity only.
func TestCreateIsColdAndRuntimeFree(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "cold", t.TempDir())
	snap := m.Snapshot()
	if snap.Harness != session.HarnessDevin || snap.NativeSessionID != "" || snap.Generation != 0 {
		t.Fatalf("cold session = %+v", snap)
	}
	if ids := acp.FakeSessionIDs(e.ACPState); len(ids) != 0 {
		t.Fatalf("cold session created native sessions: %v", ids)
	}
	if e.Supervisor.Len() != 0 {
		t.Fatalf("runtime count = %d, want 0", e.Supervisor.Len())
	}
}

// TestNativeIdentityPersistedOnlyAfterCompletedTurn: the Devin slug is
// provisional until a turn completes — the materialization rule.
func TestNativeIdentityPersistedOnlyAfterCompletedTurn(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "materialize", t.TempDir())
	res, err := a.Prompt(bg(), m, text("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeSessionID == "" {
		t.Fatal("the prompt must report the native identity in force")
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	e.WaitDurable(m.Session.ID, api.EventNativeSession)

	got := e.Reload(m.Session.ID)
	if got.NativeSessionID != res.NativeSessionID {
		t.Fatalf("persisted slug = %q, want %q", got.NativeSessionID, res.NativeSessionID)
	}
	// generation 0 → 1 at the first native BINDING (not at materialization).
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	if got.Mode != BypassMode {
		t.Fatalf("mode = %q, want the explicitly selected bypass mode", got.Mode)
	}
	if !a.State(m.Session.ID).Materialized {
		t.Fatal("adapter must consider the session materialized")
	}
}

// TestBypassModeSelectedExplicitly: Relay selects the advertised bypass mode
// through the native config surface rather than assuming the runtime's default.
func TestBypassModeSelectedExplicitly(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "bypass", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hello")); err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if n := acp.CountFakeEvents(e.ACPState, "set_config_option"); n != 1 {
		t.Fatalf("native mode selections = %d, want 1", n)
	}
	joined := ""
	for _, line := range acp.ReadFakeEvents(e.ACPState) {
		joined += line + "\n"
	}
	if !contains(joined, "mode=bypass") {
		t.Fatalf("events = %v", acp.ReadFakeEvents(e.ACPState))
	}
}

// TestExactColdResumeUsesSessionLoad: a proven slug is resumed exactly, and the
// generation advances.
func TestExactColdResumeUsesSessionLoad(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "resume", t.TempDir())
	first, err := a.Prompt(bg(), m, text("first"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventNativeSession)
	harnessenv.WaitFor(t, "turn settled", func() bool { return turnSettled(e, a, m.Session.ID) })
	if err := e.Supervisor.Stop(RuntimeKeyFor("")); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "COLD after runtime stop", func() bool { return !a.State(m.Session.ID).Live })

	second, err := a.Prompt(bg(), m, text("second"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	if second.NativeSessionID != first.NativeSessionID {
		t.Fatalf("resumed %q, want the exact %q", second.NativeSessionID, first.NativeSessionID)
	}
	if got := e.Reload(m.Session.ID).Generation; got != 2 {
		t.Fatalf("generation = %d, want 2 (first bind + exact resume)", got)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != 1 {
		t.Fatalf("session/load calls = %d, want 1", n)
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (no substitution)", n)
	}
	var resumed api.HarnessStartedPayload
	started := e.DurablePayloads(m.Session.ID, api.EventHarnessStarted)
	if err := json.Unmarshal(started[len(started)-1], &resumed); err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.Model != "" {
		t.Fatalf("harness.started = %+v", resumed)
	}
}

// TestZeroTurnSessionIsNotResumed: a Devin session with no completed turn has
// no resumable identity, so the next prompt creates a new native session and
// the durable identity is only ever the proven one.
func TestZeroTurnSessionIsNotResumed(t *testing.T) {
	e, a := newEnv(t, acp.FakeHappy)
	m := newSession(t, e, a, "zeroturn", t.TempDir())

	// Materialize natively without completing a turn, then kill the runtime.
	srv, key, err := a.ensureRuntime(bg(), m.Snapshot().Cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := acp.OpenSession(bg(), srv, m.Snapshot().Cwd)
	if err != nil {
		t.Fatal(err)
	}
	unproven := handle.ID()
	if err := a.bind(m.Session.ID, key, srv, handle); err != nil {
		t.Fatal(err)
	}
	if e.Reload(m.Session.ID).NativeSessionID != "" {
		t.Fatal("an unproven slug must never be persisted")
	}
	if err := e.Supervisor.Stop(key); err != nil {
		t.Fatal(err)
	}
	harnessenv.WaitFor(t, "COLD", func() bool { return !a.State(m.Session.ID).Live })
	if st := a.State(m.Session.ID); st.NativeSessionID != "" {
		t.Fatalf("an unproven slug must not survive its generation: %+v", st)
	}

	res, err := a.Prompt(bg(), m, text("after death"))
	if err != nil {
		t.Fatal(err)
	}
	e.WaitDurable(m.Session.ID, api.EventNativeSession)
	if res.NativeSessionID == unproven {
		t.Fatal("an unproven identity must not be resumed")
	}
	if n := acp.CountFakeEvents(e.ACPState, "session/load"); n != 0 {
		t.Fatalf("session/load calls = %d, want 0 (nothing proven to load)", n)
	}
	if got := e.Reload(m.Session.ID).NativeSessionID; got != res.NativeSessionID {
		t.Fatalf("persisted slug = %q, want the newly proven %q", got, res.NativeSessionID)
	}
}

// TestMalformedNativeIdentityFailsClosed: a malformed durable slug never wakes
// a runtime.
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

// TestRuntimeDoesNotAdvertiseLoadFailsClosed.
func TestRuntimeDoesNotAdvertiseLoadFailsClosed(t *testing.T) {
	e, a := newEnv(t, acp.FakeNoLoad)
	m := newSession(t, e, a, "noload", t.TempDir())
	if _, err := a.Prompt(bg(), m, text("hello")); !isUnsupported(err) {
		t.Fatalf("err = %v, want UNSUPPORTED_OPERATION", err)
	}
}
