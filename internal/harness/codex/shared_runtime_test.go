package codex

import (
	"context"
	"sync"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestObserverDoesNotWakeColdRuntime: a live event subscriber on a COLD
// session must not start or retain any runtime for it.
func TestObserverDoesNotWakeColdRuntime(t *testing.T) {
	e := newEnv(t, "happy")
	observer := e.newSession("observer", t.TempDir())
	active := e.newSession("active", t.TempDir())

	evs, ok := e.broker.Get(observer.Session.ID)
	if !ok {
		t.Fatal("observer session has no event state")
	}
	sub, err := evs.Subscribe(evs.Cursor())
	if err != nil {
		t.Fatal(err)
	}
	defer evs.Unsubscribe(sub)

	if _, err := e.adapter.Prompt(context.Background(), active, "wake", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(active.Session.ID, api.EventMessageAgentCompleted)

	if _, ok := e.sup.View(observer.Session.ID); ok {
		t.Fatal("observer session gained a runtime binding")
	}
	if e.sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 (only the active session)", e.sup.Len())
	}
	if types := e.durableTypes(observer.Session.ID); len(types) != 0 {
		t.Fatalf("observer session received events: %v", types)
	}
	// The observer can still be prompted later — its own runtime wake.
	if _, err := e.adapter.Prompt(context.Background(), observer, "later", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(observer.Session.ID, api.EventMessageAgentCompleted)
	if _, ok := e.sup.View(observer.Session.ID); !ok {
		t.Fatal("observer session must wake for its own prompt")
	}
}

// TestMultiSessionSharedRuntimeAndDeleteIsolation: several sessions share
// one app-server process; deleting one leaves the others working.
func TestMultiSessionSharedRuntimeAndDeleteIsolation(t *testing.T) {
	e := newEnv(t, "happy")
	sessions := make([]*session.Managed, 0, 3)
	for _, key := range []string{"s1", "s2", "s3"} {
		sessions = append(sessions, e.newSession(key, t.TempDir()))
	}
	for i, m := range sessions {
		if _, err := e.adapter.Prompt(context.Background(), m, "hi", "", ""); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
		e.waitDurable(m.Session.ID, api.EventMessageAgentCompleted)
	}
	if e.sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 shared app-server", e.sup.Len())
	}
	pids := map[int]bool{}
	for _, m := range sessions {
		v, ok := e.sup.View(m.Session.ID)
		if !ok {
			t.Fatalf("session %s has no runtime", m.Session.Key)
		}
		pids[v.PID] = true
	}
	if len(pids) != 1 {
		t.Fatalf("sessions do not share one process: %v", pids)
	}
	// Three distinct native threads, each durably recorded.
	native := map[string]bool{}
	for _, m := range sessions {
		id := e.reload(m.Session.ID).NativeSessionID
		if id == "" || native[id] {
			t.Fatalf("session %s native id %q (duplicate or empty)", m.Session.Key, id)
		}
		native[id] = true
	}
	// Delete one session: the shared runtime survives and the others keep
	// their exact identity.
	victim := sessions[1]
	e.adapter.StopSession(victim)
	if err := e.sup.StopSession(victim.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(victim.Session.ID); err != nil {
		t.Fatal(err)
	}
	e.broker.Remove(victim.Session.ID)
	if e.sup.Len() != 1 {
		t.Fatal("shared runtime died with one session")
	}
	for _, m := range []*session.Managed{sessions[0], sessions[2]} {
		if _, ok := e.sup.View(m.Session.ID); !ok {
			t.Fatalf("session %s lost its runtime binding", m.Session.Key)
		}
		before := e.reload(m.Session.ID).NativeSessionID
		if _, err := e.adapter.Prompt(context.Background(), m, "more", "", ""); err != nil {
			t.Fatalf("session %s broken after sibling delete: %v", m.Session.Key, err)
		}
		waitFor(t, "idle", func() bool { return e.adapter.Idle(m.Session.ID) })
		if after := e.reload(m.Session.ID).NativeSessionID; after != before {
			t.Fatalf("session %s native identity drifted %q -> %q", m.Session.Key, before, after)
		}
	}
}

// TestConcurrentColdWakeSpawnsOneProcess: two sessions prompting at the
// same instant on a COLD runtime must converge on one app-server process
// (claim-first creation) with two distinct native threads.
func TestConcurrentColdWakeSpawnsOneProcess(t *testing.T) {
	e := newEnv(t, "happy")
	first := e.newSession("wake1", t.TempDir())
	second := e.newSession("wake2", t.TempDir())

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, m := range []*session.Managed{first, second} {
		wg.Add(1)
		go func(i int, m *session.Managed) {
			defer wg.Done()
			_, errs[i] = e.adapter.Prompt(context.Background(), m, "hi", "", "")
		}(i, m)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent prompt %d: %v", i, err)
		}
	}
	e.waitDurable(first.Session.ID, api.EventMessageAgentCompleted)
	e.waitDurable(second.Session.ID, api.EventMessageAgentCompleted)
	if e.sup.Len() != 1 {
		t.Fatalf("runtimes = %d, want exactly 1", e.sup.Len())
	}
	va, ok := e.sup.View(first.Session.ID)
	if !ok {
		t.Fatal("first session has no runtime")
	}
	vb, ok := e.sup.View(second.Session.ID)
	if !ok {
		t.Fatal("second session has no runtime")
	}
	if va.PID != vb.PID {
		t.Fatalf("two processes were spawned: %d vs %d", va.PID, vb.PID)
	}
	idA := e.reload(first.Session.ID).NativeSessionID
	idB := e.reload(second.Session.ID).NativeSessionID
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("native threads = %q / %q", idA, idB)
	}
}
