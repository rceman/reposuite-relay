package session

import (
	"errors"
	"testing"
	"time"
)

func TestValidKey(t *testing.T) {
	valid := []string{"test", "worker_1", "project_master", "repo.agent-2",
		"A", "0", "a.b-c_d", stringOfLen(64)}
	for _, k := range valid {
		if !ValidKey(k) {
			t.Errorf("%q must be valid", k)
		}
	}
	invalid := []string{"", ".hidden", "-x", "_x", "a/b", `a\b`, "a b",
		"a\tb", "a\nb", "a\x00b", "ünïcode", stringOfLen(65), "!bang", "key?"}
	for _, k := range invalid {
		if ValidKey(k) {
			t.Errorf("%q must be invalid", k)
		}
	}
}

func stringOfLen(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		sid, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		rid, err := NewRuntimeID()
		if err != nil {
			t.Fatalf("NewRuntimeID: %v", err)
		}
		for _, id := range []string{sid, rid} {
			if !ValidID(id) {
				t.Fatalf("id %q malformed", id)
			}
			if seen[id] {
				t.Fatalf("duplicate id %q", id)
			}
			seen[id] = true
		}
	}
}

func mkRelay(key string) *RelaySession {
	id, _ := NewSessionID()
	now := time.Now()
	return &RelaySession{ID: id, Key: key, Harness: HarnessFixture,
		Cwd: "/", State: StateIdle, Generation: 1,
		CreatedAt: now, UpdatedAt: now}
}

func mkManaged(k string) *Managed {
	now := time.Now()
	rid, _ := NewRuntimeID()
	return &Managed{
		Session: mkRelay(k),
		Runtime: &HarnessRuntime{ID: rid, Generation: 1, State: RuntimeWarm,
			PID: 100 + len(k), StartedAt: now},
	}
}

func TestRegistryReserveCommitCancel(t *testing.T) {
	r := NewRegistry()
	if !r.Reserve("k") {
		t.Fatal("reserve failed")
	}
	if r.Reserve("k") {
		t.Fatal("double reserve must fail")
	}
	r.Cancel("k")
	if !r.Reserve("k") {
		t.Fatal("reserve after cancel must succeed")
	}
	if !r.Commit("k", mkManaged("k")) {
		t.Fatal("commit failed")
	}
	if r.Reserve("k") {
		t.Fatal("reserve on committed key must fail")
	}
	if r.Commit("other", mkManaged("other")) {
		t.Fatal("commit without reservation must fail")
	}
}

func TestRegistryStopOwnership(t *testing.T) {
	r := NewRegistry()
	if !r.Reserve("a") || !r.Commit("a", mkManaged("a")) {
		t.Fatal("setup failed")
	}
	// Losers of the stop race get !ok and never see the Managed.
	m, ok := r.BeginStop("a")
	if !ok {
		t.Fatal("BeginStop failed")
	}
	if _, ok := r.BeginStop("a"); ok {
		t.Fatal("second BeginStop must fail")
	}
	// During stop the session still exists.
	if g, ok := r.Get("a"); !ok || g.Session.State != StateIdle {
		t.Fatal("stopping session must remain queryable")
	}
	// Reserve on a stopping key fails — the key is still owned.
	if r.Reserve("a") {
		t.Fatal("reserve during stop must fail")
	}
	r.AbortStop("a")
	if _, ok := r.BeginStop("a"); !ok {
		t.Fatal("BeginStop after abort must succeed")
	}
	r.CommitStop("a")
	if _, ok := r.Get("a"); ok {
		t.Fatal("session must be gone after CommitStop")
	}
	if _, ok := r.BeginStop("a"); ok {
		t.Fatal("BeginStop on removed key must fail")
	}
	// Key is free again after a completed stop.
	if !r.Reserve("a") {
		t.Fatal("key must be re-creatable after CommitStop")
	}
	r.Cancel("a")
	_ = m
}

// TestStopFailureRetainsAuthority: a failed Stop must NOT erase the
// logical session — the daemon keeps authority over the still-running
// runtime.
func TestStopFailureRetainsAuthority(t *testing.T) {
	r := NewRegistry()
	m := mkManaged("victim")
	stopped := false
	m.Stop = func() error {
		if !stopped {
			return errors.New("stop failed")
		}
		return nil
	}
	if !r.Reserve("victim") || !r.Commit("victim", m) {
		t.Fatal("setup failed")
	}
	got, ok := r.BeginStop("victim")
	if !ok {
		t.Fatal("BeginStop failed")
	}
	if err := got.Stop(); err == nil {
		t.Fatal("injected stop must fail")
	}
	r.AbortStop("victim")
	// Authority retained: same session/runtime still queryable.
	g, ok := r.Get("victim")
	if !ok || g.Session.ID != m.Session.ID || g.Runtime == nil || g.Runtime.ID != m.Runtime.ID {
		t.Fatal("session authority lost after failed stop")
	}
	// Retry succeeds: runtime stopped, cleared to COLD, then removed.
	got, ok = r.BeginStop("victim")
	if !ok {
		t.Fatal("second BeginStop failed")
	}
	stopped = true
	if err := got.Stop(); err != nil {
		t.Fatal(err)
	}
	r.ClearRuntime("victim")
	if g, ok := r.Get("victim"); !ok || g.Runtime != nil || g.Stop != nil {
		t.Fatal("ClearRuntime must detach runtime and stop hook")
	}
	r.CommitStop("victim")
	if _, ok := r.Get("victim"); ok {
		t.Fatal("session must be gone after successful stop")
	}
}

// TestClearRuntimeRequiresOwnership: only the stop owner may transition a
// session to COLD — a plain Get must not be able to detach the runtime.
func TestClearRuntimeRequiresOwnership(t *testing.T) {
	r := NewRegistry()
	m := mkManaged("k")
	if !r.Reserve("k") || !r.Commit("k", m) {
		t.Fatal("setup failed")
	}
	r.ClearRuntime("k") // no stop ownership — must be a no-op
	if g, _ := r.Get("k"); g.Runtime == nil {
		t.Fatal("ClearRuntime without ownership must not detach runtime")
	}
}

func TestRegistryOps(t *testing.T) {
	r := NewRegistry()
	for _, k := range []string{"b", "a"} {
		if !r.Reserve(k) || !r.Commit(k, mkManaged(k)) {
			t.Fatal("create failed")
		}
	}
	list := r.List()
	if len(list) != 2 || list[0].Session.Key != "a" || list[1].Session.Key != "b" {
		t.Fatalf("list not sorted: %+v", list)
	}
	m, ok := r.Get("a")
	if !ok || m.Session.ID == "" || m.Runtime == nil || m.Runtime.PID == 0 {
		t.Fatal("get a failed")
	}
	if _, ok := r.Get("zzz"); ok {
		t.Fatal("get unknown must fail")
	}
	if _, ok := r.BeginStop("zzz"); ok {
		t.Fatal("stop unknown must fail")
	}
	if _, ok := r.BeginStop("a"); !ok {
		t.Fatal("beginstop a failed")
	}
	r.CommitStop("a")
	if r.Len() != 1 {
		t.Fatalf("len=%d", r.Len())
	}
	dr := r.Drain()
	if len(dr) != 1 || r.Len() != 0 {
		t.Fatalf("drain: %d remain", r.Len())
	}
}

// TestNewRestoredCold: persisted sessions restore as managed COLD
// entries — queryable, countable, with no runtime and no stop hook.
func TestNewRestoredCold(t *testing.T) {
	sessions := []*RelaySession{mkRelay("a"), mkRelay("b")}
	r, err := NewRestored(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Fatalf("len=%d", r.Len())
	}
	m, ok := r.Get("a")
	if !ok || m.Session.ID != sessions[0].ID {
		t.Fatal("restored session a missing")
	}
	if m.Runtime != nil || m.Stop != nil {
		t.Fatal("restored session must be COLD (no runtime, no stop hook)")
	}
	// A COLD session's key is still owned — no duplicate create.
	if r.Reserve("a") {
		t.Fatal("reserve over restored key must fail")
	}
	// And it is stoppable: BeginStop works without a runtime.
	if _, ok := r.BeginStop("a"); !ok {
		t.Fatal("BeginStop on COLD restored session must succeed")
	}
	r.CommitStop("a")
	if _, ok := r.Get("a"); ok {
		t.Fatal("cold session must be gone after stop")
	}
}

// TestNewRestoredRejectsDuplicates: duplicate persisted keys or IDs are a
// startup error — never silently discarded.
func TestNewRestoredRejectsDuplicates(t *testing.T) {
	a, b := mkRelay("a"), mkRelay("a") // dup key, distinct ids
	if _, err := NewRestored([]*RelaySession{a, b}); err == nil {
		t.Fatal("duplicate key must fail")
	}
	c, d := mkRelay("c"), mkRelay("d")
	d.ID = c.ID // dup id
	if _, err := NewRestored([]*RelaySession{c, d}); err == nil {
		t.Fatal("duplicate id must fail")
	}
	bad := mkRelay("bad")
	bad.ID = "not-an-id"
	if _, err := NewRestored([]*RelaySession{bad}); err == nil {
		t.Fatal("invalid id must fail")
	}
	badKey := mkRelay("x")
	badKey.Key = "bad key!"
	if _, err := NewRestored([]*RelaySession{badKey}); err == nil {
		t.Fatal("invalid key must fail")
	}
}
