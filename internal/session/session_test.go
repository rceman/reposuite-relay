package session

import (
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
		id := NewRuntimeID()
		if len(id) != 32 {
			t.Fatalf("runtimeId %q length %d", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate runtimeId %q", id)
		}
		seen[id] = true
	}
}

func TestRegistryOps(t *testing.T) {
	r := NewRegistry()
	mk := func(k string) *Managed {
		now := time.Now()
		return &Managed{
			Session:    &Session{Key: k, RuntimeID: NewRuntimeID(), Harness: HarnessFixture, State: StateRunning, CreatedAt: now, Generation: 1},
			Generation: &ActiveGeneration{PID: 100 + len(k), StartedAt: now},
		}
	}
	if !r.Create(mk("b")) || !r.Create(mk("a")) {
		t.Fatal("create failed")
	}
	if r.Create(mk("a")) {
		t.Fatal("duplicate create must fail")
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
	if _, ok := r.Remove("zzz"); ok {
		t.Fatal("remove unknown must fail")
	}
	if _, ok := r.Remove("a"); !ok {
		t.Fatal("remove a failed")
	}
	if r.Len() != 1 {
		t.Fatalf("len=%d", r.Len())
	}
	dr := r.Drain()
	if len(dr) != 1 || r.Len() != 0 {
		t.Fatalf("drain: %d remain", r.Len())
	}
}
