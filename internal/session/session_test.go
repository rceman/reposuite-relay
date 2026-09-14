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

func TestRuntimeIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id, err := NewRuntimeID()
		if err != nil {
			t.Fatalf("NewRuntimeID: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("runtimeId %q length %d", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate runtimeId %q", id)
		}
		seen[id] = true
	}
}

func mkManaged(k string) *Managed {
	now := time.Now()
	id, _ := NewRuntimeID()
	return &Managed{
		Session:    &Session{Key: k, RuntimeID: id, Harness: HarnessFixture, State: StateRunning, CreatedAt: now, Generation: 1},
		Generation: &ActiveGeneration{PID: 100 + len(k), StartedAt: now},
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
	// During stop the session still exists and still reports running.
	if g, ok := r.Get("a"); !ok || g.Session.State != StateRunning {
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
// generation.
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
	// Authority retained: same runtimeId/generation still queryable.
	g, ok := r.Get("victim")
	if !ok || g.Session.RuntimeID != m.Session.RuntimeID || g.Session.Generation != 1 {
		t.Fatal("session authority lost after failed stop")
	}
	// Retry succeeds and removes.
	got, ok = r.BeginStop("victim")
	if !ok {
		t.Fatal("second BeginStop failed")
	}
	stopped = true
	if err := got.Stop(); err != nil {
		t.Fatal(err)
	}
	r.CommitStop("victim")
	if _, ok := r.Get("victim"); ok {
		t.Fatal("session must be gone after successful stop")
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
	if !ok || m.Session.RuntimeID == "" || m.Generation.PID == 0 {
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
