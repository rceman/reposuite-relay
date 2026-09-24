package codex

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"testing"
	"time"
)

// env is a minimal daemon-side harness for adapter tests: a real store, a
// real event broker, a real RuntimeSupervisor, and a real spawned fake
// app-server process. No model call is ever made.
type env struct {
	t       *testing.T
	store   *store.Sessions
	broker  *events.Broker
	sup     *runtime.Supervisor
	adapter *Adapter
}

func newEnv(t *testing.T, mode string) *env {
	t.Helper()
	root := t.TempDir()
	ss, err := store.OpenSessions(root, session.NewSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sup := runtime.New()
	e := &env{
		t:      t,
		store:  ss,
		broker: events.NewBroker(ss),
		sup:    sup,
	}
	e.adapter = NewAdapter(Deps{
		Broker:      e.broker,
		Supervisor:  sup,
		Command:     fakeCommand,
		Materialize: e.materialize,
		Version:     "test",
	})
	sup.OnGone = e.adapter.OnRuntimeGone
	t.Setenv("RELAY_FAKE_APP_SERVER", "1")
	t.Setenv("FAKE_CODEX_MODE", mode)
	// StopAll runs BEFORE the temp root is removed (t.Cleanup is LIFO, and
	// t.TempDir was registered first). The drain order is deterministic:
	// reap every runtime, wait for crash watchers to finish OnRuntimeGone,
	// then wait for every transport reader goroutine to exit — after that
	// no adapter goroutine can still write into the temp root. (Tests may
	// swap e.sup, so the current supervisor is drained, not the captured
	// one.)
	t.Cleanup(func() {
		_ = e.sup.StopAll()
		e.sup.WaitWatchers()
		e.adapter.WaitQuiescent()
		waitFor(t, "in-flight turns to settle", func() bool {
			return e.adapter.turnsInFlight() == 0
		})
	})
	return e
}

// swapSupervisor replaces the env's supervisor with a fresh one — a daemon
// restart shape. It first drains the old generation completely: runtimes
// stopped and reaped, crash watchers finished, transport readers exited —
// so no goroutine can still read deps.Supervisor while it is swapped.
func (e *env) swapSupervisor() {
	e.t.Helper()
	_ = e.sup.StopAll()
	e.sup.WaitWatchers()
	e.adapter.WaitQuiescent()
	e.sup = runtime.New()
	e.sup.OnGone = e.adapter.OnRuntimeGone
	e.adapter.deps.Supervisor = e.sup
}

// materialize mirrors the daemon's durable metadata mutation.
func (e *env) materialize(m *session.Managed, upd harness.SessionUpdate) error {
	m.MetaMu.Lock()
	defer m.MetaMu.Unlock()
	rs := m.Session
	if upd.NativeSessionID != nil {
		rs.NativeSessionID = *upd.NativeSessionID
	}
	if upd.Model != nil {
		rs.Model = *upd.Model
	}
	if upd.Mode != nil {
		rs.Mode = *upd.Mode
	}
	if upd.State != nil {
		rs.State = *upd.State
	}
	if upd.BumpGeneration {
		rs.Generation++
	}
	if upd.TelemetrySeqWatermark != nil &&
		*upd.TelemetrySeqWatermark > rs.TelemetrySeqWatermark {
		rs.TelemetrySeqWatermark = *upd.TelemetrySeqWatermark
	}
	rs.UpdatedAt = time.Now().UTC()
	return e.store.Save(rs)
}

// newSession creates one durable Codex session bound to the adapter.
func (e *env) newSession(key, cwd string) *session.Managed {
	e.t.Helper()
	id, err := session.NewSessionID()
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC()
	rs := &session.RelaySession{
		ID: id, Key: key, Harness: session.HarnessCodex, Cwd: cwd,
		State: session.StateIdle, Generation: 0, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.store.Create(rs); err != nil {
		e.t.Fatal(err)
	}
	m := &session.Managed{Session: rs}
	if _, err := e.broker.Ensure(m); err != nil {
		e.t.Fatal(err)
	}
	e.adapter.Track(m)
	return m
}

// durableTypes returns the durable transcript event types in order.
func (e *env) durableTypes(id string) []string {
	e.t.Helper()
	tr, err := e.store.Transcript(id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Type)
	}
	return out
}

// durablePayload returns the payload of the last durable record of a type.
func (e *env) durablePayload(id, typ string) json.RawMessage {
	e.t.Helper()
	tr, err := e.store.Transcript(id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		e.t.Fatal(err)
	}
	var found json.RawMessage
	for _, r := range recs {
		if r.Type == typ {
			found = r.Payload
		}
	}
	return found
}

func (e *env) reload(id string) *session.RelaySession {
	e.t.Helper()
	all, err := e.store.LoadAll()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, rs := range all {
		if rs.ID == id {
			return rs
		}
	}
	e.t.Fatalf("session %s not on disk", id)
	return nil
}

// waitDurable waits until a durable record of the given type exists: the
// canonical event stream is the deterministic ordering point, so state
// asserted after this is stable.
func (e *env) waitDurable(id, typ string) {
	e.t.Helper()
	waitFor(e.t, "durable "+typ, func() bool {
		return e.durablePayload(id, typ) != nil
	})
}

// waitFor polls until cond holds or the deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
