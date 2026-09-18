package daemon

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestCodexPromptLifecycleOverHTTP: create (COLD) → prompt → durable
// canonical events → status projection → history replay.
func TestCodexPromptLifecycleOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)

	info := serveCodex(t, c, "sess")
	if info.RuntimeState != session.RuntimeCold || info.RuntimeID != "" || info.PID != 0 {
		t.Fatalf("create must be COLD: %+v", info)
	}
	if info.Harness != session.HarnessCodex || info.NativeSessionID != "" || info.Generation != 0 {
		t.Fatalf("fresh session = %+v", info)
	}

	ctx, cancel := tctx(t)
	defer cancel()
	res, err := c.Prompt(ctx, "sess", "hello world", "", "")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.TurnID == "" || res.NativeSessionID == "" || res.RuntimeID == "" || res.RuntimeID == codex.RuntimeKey {
		t.Fatalf("prompt response = %+v", res)
	}

	page := waitDurableTypes(t, c, "sess", api.EventMessageAgentCompleted)
	types := make([]string, 0, len(page.Records))
	for _, r := range page.Records {
		types = append(types, r.Type)
	}
	want := []string{api.EventMessageUser, api.EventHarnessStarted,
		api.EventNativeSession, api.EventMessageAgentCompleted}
	if len(types) != len(want) {
		t.Fatalf("durable types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("durable types = %v, want %v", types, want)
		}
	}

	// The session is now materialized: exact native identity, generation 1
	// (create 0 -> first native binding 1; materialization does not bump).
	ctx2, cancel2 := tctx(t)
	defer cancel2()
	st, err := c.Status(ctx2, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.NativeSessionID != res.NativeSessionID || st.Session.Generation != 1 {
		t.Fatalf("status = %+v", st.Session)
	}
	if st.Session.State != session.StateIdle || st.Session.Activity != "idle" {
		t.Fatalf("session not idle after completion: %+v", st.Session)
	}
	if st.Session.RuntimeID == "" || st.Session.RuntimeID != res.RuntimeID || st.Session.RuntimeState != session.RuntimeWarm {
		t.Fatalf("runtime not warm after a turn or id mismatch: %+v vs %q", st.Session, res.RuntimeID)
	}
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1", d.supervisor.Len())
	}
}

// TestCodexHistoryAndLiveCutover: a client that hydrates history and then
// attaches with the history cursor receives exactly the later events —
// no gap, no duplicate.
func TestCodexHistoryAndLiveCutover(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	_, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "cut")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "cut", "one", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "cut", api.EventMessageAgentCompleted)

	stream, err := c.Events(ctx, "cut", page.ThroughSeq)
	if err != nil {
		t.Fatalf("events after throughSeq=%d: %v", page.ThroughSeq, err)
	}
	defer stream.Close()
	if _, err := c.Prompt(ctx, "cut", "two", "", ""); err != nil {
		t.Fatal(err)
	}
	// The stream resumes strictly after throughSeq: transient events may
	// appear first, but the next durable prompt record must be the second
	// prompt's message.user with a greater seq and no gap.
	var ev events.Event
	for {
		next, err := stream.Next()
		if err != nil {
			t.Fatalf("stream next: %v", err)
		}
		if next.Seq <= page.ThroughSeq {
			t.Fatalf("replayed an already-hydrated event: %+v (throughSeq=%d)", next, page.ThroughSeq)
		}
		if next.Durable {
			ev = next
			break
		}
	}
	if ev.Type != api.EventMessageUser {
		t.Fatalf("first durable cutover event = %s, want %s", ev.Type, api.EventMessageUser)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Text != "two" {
		t.Fatalf("cutover text = %q", payload.Text)
	}
}

// TestCodexDaemonRestartExactResume: Relay sessions and the exact native
// identity survive a daemon restart; the runtime does not.
func TestCodexDaemonRestartExactResume(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)
	first, err := Start(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- first.Serve() }()
	c := dialClient(t, p)
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.CreateSession(ctx, session.HarnessCodex, "restart", t.TempDir(), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prompt(ctx, "restart", "hello", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "restart", api.EventMessageAgentCompleted)
	st, err := c.Status(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	native := st.Session.NativeSessionID
	pid := st.Session.PID
	if native == "" || pid == 0 {
		t.Fatalf("not materialized: %+v", st.Session)
	}
	first.Shutdown()
	select {
	case <-served:
	case <-time.After(20 * time.Second):
		t.Fatal("first daemon did not stop")
	}
	waitProcDead(t, pid, 5*time.Second)

	// Second daemon generation: the session is restored COLD with its
	// exact native identity intact.
	second, err := Start(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Shutdown()
	served2 := make(chan error, 1)
	go func() { served2 <- second.Serve() }()
	c2 := dialClient(t, p)
	ctx2, cancel2 := tctx(t)
	defer cancel2()
	st2, err := c2.Status(ctx2, "restart")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Session.NativeSessionID != native {
		t.Fatalf("native identity lost across restart: %q", st2.Session.NativeSessionID)
	}
	if st2.Session.RuntimeID != "" || st2.Session.PID != 0 || st2.Session.RuntimeState != session.RuntimeCold {
		t.Fatalf("restored session must be COLD: %+v", st2.Session)
	}
	if second.supervisor.Len() != 0 {
		t.Fatal("restart must not spawn runtimes")
	}
	// The next prompt resumes the exact native thread in a new runtime.
	if _, err := c2.Prompt(ctx2, "restart", "after restart", "", ""); err != nil {
		t.Fatalf("prompt after restart: %v", err)
	}
	page := waitDurableTypes(t, c2, "restart", api.EventMessageAgentCompleted)
	var resumedID string
	for _, r := range page.Records {
		if r.Type != api.EventHarnessStarted {
			continue
		}
		var started struct {
			NativeSessionID string `json:"nativeSessionId"`
			Resumed         bool   `json:"resumed"`
		}
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.Resumed {
			resumedID = started.NativeSessionID
		}
	}
	if resumedID != native {
		t.Fatalf("resumed %q, want the exact %q", resumedID, native)
	}
}

// TestCodexRuntimeProcessTreeReaped: stopping the runtime kills the whole
// process tree (the app-server's own child included).
func TestCodexRuntimeProcessTreeReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CODEX_MODE", "child")
	t.Setenv("FAKE_CODEX_CHILDPID_FILE", pidFile)
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "tree")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "tree", "hi", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "tree", api.EventMessageAgentCompleted)

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("grandchild pid file: %v", err)
	}
	grandchild, err := strconv.Atoi(string(raw))
	if err != nil || grandchild <= 0 {
		t.Fatalf("bad grandchild pid %q", raw)
	}
	if !procAlive(grandchild) {
		t.Fatal("grandchild not running")
	}
	if err := d.supervisor.Stop(codex.RuntimeKey); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	waitProcDead(t, grandchild, 5*time.Second)
	if d.supervisor.Len() != 0 {
		t.Fatal("runtime still registered after stop")
	}
	// The session survives the runtime stop as COLD.
	st, err := c.Status(ctx, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.RuntimeState != session.RuntimeCold || st.Session.NativeSessionID == "" {
		t.Fatalf("status after runtime stop: %+v", st.Session)
	}
}
