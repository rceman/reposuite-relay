// Package harnessenv is test-only support shared by the native harness adapter
// test suites (Codex's own suite predates it and keeps its local env).
//
// It is a real package rather than a _test.go file because three suites need
// the same concrete scaffolding: a real store, a real event broker, a real
// RuntimeSupervisor, the durable-metadata mutation hook, and a spawned
// deterministic fake ACP agent. It performs no model call, no network access,
// and consumes no quota.
package harnessenv

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

var (
	binOnce sync.Once
	binPath string
	binDir  string
)

// Main builds the fake-agent binary once, runs the tests, and cleans up. A
// test package enables the fake ACP seam with:
//
//	func TestMain(m *testing.M) { os.Exit(harnessenv.Main(m)) }
func Main(m *testing.M) int {
	binOnce.Do(build)
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	return code
}

func build() {
	dir, err := os.MkdirTemp("", "relay-harness-test-bin")
	if err != nil {
		panic(err)
	}
	binDir = dir
	binPath = filepath.Join(dir, "reposuite-relay")
	out, err := exec.Command("go", "build", "-o", binPath,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build test binary: %v\n%s", err, out))
	}
}

// Bin returns the built binary path (building it if Main was not used).
func Bin(t *testing.T) string {
	t.Helper()
	binOnce.Do(build)
	return binPath
}

// FakeACPCommand returns a command that runs the deterministic fake ACP agent
// for one vendor and scenario. The fake state directory is per-command, so the
// "native" session store is genuinely isolated per test.
func FakeACPCommand(t *testing.T, vendor, mode, stateDir string) func() (acp.Command, error) {
	t.Helper()
	bin := Bin(t)
	env := append(os.Environ(),
		acp.EnvFakeACPVendor+"="+vendor,
		acp.EnvFakeACPMode+"="+mode,
		acp.EnvFakeACPState+"="+stateDir,
	)
	return func() (acp.Command, error) {
		return acp.Command{Path: bin, Args: []string{"__fake-acp"}, Env: env}, nil
	}
}

// Env is the daemon-side scaffolding one adapter test suite needs.
type Env struct {
	T          *testing.T
	Store      *store.Sessions
	Broker     *events.Broker
	Supervisor *runtime.Supervisor
	// ACPState is the fake agent's native store root (shared by every runtime
	// generation the test spawns, which is what makes exact resume real).
	ACPState string
	// Drain, when set by the adapter under test, blocks until no
	// adapter-owned goroutine can still write durable state (in-flight
	// turn completion, transport readers). It runs in cleanup AFTER all
	// runtimes are stopped and reaped, so no async write can race the
	// TempDir removal that follows the test.
	Drain func()
}

// New builds the scaffolding. StopAll runs on cleanup.
func New(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
	ss, err := store.OpenSessions(root, session.NewSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sup := runtime.New()
	e := &Env{
		T:          t,
		Store:      ss,
		Broker:     events.NewBroker(ss),
		Supervisor: sup,
		ACPState:   filepath.Join(t.TempDir(), "acp-state"),
	}
	t.Cleanup(func() {
		_ = sup.StopAll()
		sup.WaitWatchers()
		if e.Drain != nil {
			e.Drain()
		}
	})
	return e
}

// Materialize mirrors the daemon's durable metadata mutation exactly.
func (e *Env) Materialize(m *session.Managed, upd harness.SessionUpdate) error {
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
	rs.UpdatedAt = time.Now().UTC()
	return e.Store.Save(rs)
}

// NewSession creates one durable session for a harness and tracks it with the
// adapter supplied by the caller.
func (e *Env) NewSession(key, harnessName, cwd string, generation int, track func(*session.Managed)) *session.Managed {
	e.T.Helper()
	id, err := session.NewSessionID()
	if err != nil {
		e.T.Fatal(err)
	}
	now := time.Now().UTC()
	rs := &session.RelaySession{
		ID: id, Key: key, Harness: harnessName, Cwd: cwd,
		State: session.StateIdle, Generation: generation, CreatedAt: now, UpdatedAt: now,
	}
	if err := e.Store.Create(rs); err != nil {
		e.T.Fatal(err)
	}
	m := &session.Managed{Session: rs}
	if _, err := e.Broker.Ensure(m); err != nil {
		e.T.Fatal(err)
	}
	if track != nil {
		track(m)
	}
	return m
}

// DurableTypes returns the durable transcript event types in order.
func (e *Env) DurableTypes(id string) []string {
	e.T.Helper()
	recs := e.records(id)
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Type)
	}
	return out
}

// DurablePayload returns the payload of the last durable record of a type.
func (e *Env) DurablePayload(id, typ string) json.RawMessage {
	e.T.Helper()
	var found json.RawMessage
	for _, r := range e.records(id) {
		if r.Type == typ {
			found = r.Payload
		}
	}
	return found
}

// DurablePayloads returns every payload of one durable type, in order.
func (e *Env) DurablePayloads(id, typ string) []json.RawMessage {
	e.T.Helper()
	out := []json.RawMessage{}
	for _, r := range e.records(id) {
		if r.Type == typ {
			out = append(out, r.Payload)
		}
	}
	return out
}

func (e *Env) records(id string) []store.Record {
	e.T.Helper()
	tr, err := e.Store.Transcript(id)
	if err != nil {
		e.T.Fatal(err)
	}
	defer tr.Close()
	recs, _, _, err := tr.TailBounded(1000, api.HistoryPageMaxBytes)
	if err != nil {
		e.T.Fatal(err)
	}
	return recs
}

// Reload reads the durable session from disk.
func (e *Env) Reload(id string) *session.RelaySession {
	e.T.Helper()
	all, err := e.Store.LoadAll()
	if err != nil {
		e.T.Fatal(err)
	}
	for _, rs := range all {
		if rs.ID == id {
			return rs
		}
	}
	e.T.Fatalf("session %s not on disk", id)
	return nil
}

// WaitDurable waits until a durable record of the given type exists.
func (e *Env) WaitDurable(id, typ string) {
	e.T.Helper()
	WaitFor(e.T, "durable "+typ, func() bool {
		return e.DurablePayload(id, typ) != nil
	})
}

// WaitFor polls until cond holds or the deadline expires.
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
