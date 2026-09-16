package daemon

import (
	"github.com/rceman/reposuite-relay/internal/api"
	"sync"
	"testing"
)

// TestCodexConcurrentColdWakeOverHTTP: two prompts racing on a COLD
// runtime produce one app-server process and two native threads.
func TestCodexConcurrentColdWakeOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "race1")
	serveCodex(t, c, "race2")
	ctx, cancel := tctx(t)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, key := range []string{"race1", "race2"} {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			_, errs[i] = c.Prompt(ctx, key, "hi", "", "")
		}(i, key)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent prompt %d: %v", i, err)
		}
	}
	waitDurableTypes(t, c, "race1", api.EventMessageAgentCompleted)
	waitDurableTypes(t, c, "race2", api.EventMessageAgentCompleted)
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1", d.supervisor.Len())
	}
	var pids []int
	native := map[string]bool{}
	for _, key := range []string{"race1", "race2"} {
		st, err := c.Status(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, st.Session.PID)
		if st.Session.NativeSessionID == "" || native[st.Session.NativeSessionID] {
			t.Fatalf("%s native id %q", key, st.Session.NativeSessionID)
		}
		native[st.Session.NativeSessionID] = true
	}
	if pids[0] != pids[1] {
		t.Fatalf("two app-server processes: %v", pids)
	}
}

// TestCodexMultiSessionSharedRuntime: several Relay sessions share one
// app-server process; deleting one leaves the others fully functional.
func TestCodexMultiSessionSharedRuntime(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	keys := []string{"m1", "m2", "m3"}
	for _, k := range keys {
		serveCodex(t, c, k)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	native := map[string]string{}
	for _, k := range keys {
		if _, err := c.Prompt(ctx, k, "hi "+k, "", ""); err != nil {
			t.Fatalf("prompt %s: %v", k, err)
		}
	}
	for _, k := range keys {
		waitDurableTypes(t, c, k, api.EventMessageAgentCompleted)
		st, err := c.Status(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if st.Session.NativeSessionID == "" {
			t.Fatalf("%s not materialized", k)
		}
		native[k] = st.Session.NativeSessionID
	}
	if len(map[string]bool{native["m1"]: true, native["m2"]: true, native["m3"]: true}) != 3 {
		t.Fatalf("native threads not distinct: %v", native)
	}
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 shared", d.supervisor.Len())
	}
	pids := map[int]bool{}
	for _, k := range keys {
		st, err := c.Status(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		pids[st.Session.PID] = true
	}
	if len(pids) != 1 {
		t.Fatalf("sessions do not share one process: %v", pids)
	}

	// Delete the middle session: the runtime survives, the others keep
	// their exact native identity and can still take turns.
	if _, err := c.StopSession(ctx, "m2"); err != nil {
		t.Fatalf("stop m2: %v", err)
	}
	if d.supervisor.Len() != 1 {
		t.Fatal("shared runtime died with one session")
	}
	if _, err := c.Status(ctx, "m2"); err == nil {
		t.Fatal("m2 must be gone")
	}
	for _, k := range []string{"m1", "m3"} {
		if _, err := c.Prompt(ctx, k, "again", "", ""); err != nil {
			t.Fatalf("prompt %s after sibling delete: %v", k, err)
		}
	}
	for _, k := range []string{"m1", "m3"} {
		waitDurableTypes(t, c, k, api.EventMessageAgentCompleted)
		st, err := c.Status(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if st.Session.NativeSessionID != native[k] {
			t.Fatalf("%s native id drifted %q -> %q", k, native[k], st.Session.NativeSessionID)
		}
	}
}

// TestCodexObserverDoesNotWakeRuntime: subscribing to a COLD session's
// event stream never starts a runtime for it.
func TestCodexObserverDoesNotWakeRuntime(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "watched")
	serveCodex(t, c, "worker")

	ctx, cancel := tctx(t)
	defer cancel()
	st, err := c.Status(ctx, "watched")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Events(ctx, "watched", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if _, err := c.Prompt(ctx, "worker", "hi", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "worker", api.EventMessageAgentCompleted)

	st2, err := c.Status(ctx, "watched")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Session.RuntimeID != "" || st2.Session.PID != 0 {
		t.Fatalf("observer session woke a runtime: %+v", st2.Session)
	}
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 (worker only)", d.supervisor.Len())
	}
	_ = st

	// And it still wakes for its own prompt.
	if _, err := c.Prompt(ctx, "watched", "later", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "watched", api.EventMessageAgentCompleted)
}
