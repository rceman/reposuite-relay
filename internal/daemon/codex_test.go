package daemon

// Codex vertical-slice integration tests: the real daemon (in-process),
// the real control plane (loopback HTTP through internal/client), and the
// deterministic fake app-server spawned through the production binary's
// `__fake-codex` mode. No model call is made anywhere.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/codex"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
)

// codexOptions points the daemon at the deterministic fake app-server.
func codexOptions(o *Options) {
	o.CodexCommand = func() (codex.Command, error) {
		return codex.Command{Path: testBinary(), Args: []string{"__fake-codex"}}, nil
	}
}

// serveCodex creates a Codex session for a fresh temporary working
// directory.
func serveCodex(t *testing.T, c *client.Client, key string) api.SessionInfo {
	t.Helper()
	cwd := t.TempDir()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.CreateCodex(ctx, key, cwd, "", "")
	if err != nil {
		t.Fatalf("create codex session: %v", err)
	}
	return resp.Session
}

// waitDurableTypes polls the transcript until a durable record of the
// given type exists. Later records may legitimately follow it (a turn's
// terminal event precedes its input.aborted cleanup), so the wait is for
// presence, not for the tail.
func waitDurableTypes(t *testing.T, c *client.Client, key, want string) api.TranscriptPage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last api.TranscriptPage
	for time.Now().Before(deadline) {
		ctx, cancel := tctx(t)
		page, err := c.Transcript(ctx, key, 100)
		cancel()
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		last = page
		for _, r := range page.Records {
			if r.Type == want {
				return page
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("durable record %q never appeared (last=%+v)", want, last.Records)
	return last
}

// TestCodexPromptLifecycleOverHTTP: create (COLD) → prompt → durable
// canonical events → status projection → history replay.
func TestCodexPromptLifecycleOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)

	info := serveCodex(t, c, "sess")
	if info.RuntimeState != session.RuntimeCold || info.RuntimeID != "" || info.PID != 0 {
		t.Fatalf("create must be COLD: %+v", info)
	}
	if info.Harness != session.HarnessCodex || info.NativeSessionID != "" || info.Generation != 1 {
		t.Fatalf("fresh session = %+v", info)
	}

	ctx, cancel := tctx(t)
	defer cancel()
	res, err := c.Prompt(ctx, "sess", "hello world", "", "")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.TurnID == "" || res.NativeThreadID == "" || res.RuntimeID != codex.RuntimeKey {
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

	// The session is now materialized: exact native identity, generation 2.
	ctx2, cancel2 := tctx(t)
	defer cancel2()
	st, err := c.Status(ctx2, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.NativeSessionID != res.NativeThreadID || st.Session.Generation != 2 {
		t.Fatalf("status = %+v", st.Session)
	}
	if st.Session.State != session.StateIdle || st.Session.Activity != "idle" {
		t.Fatalf("session not idle after completion: %+v", st.Session)
	}
	if st.Session.RuntimeID == "" || st.Session.RuntimeState != session.RuntimeWarm {
		t.Fatalf("runtime not warm after a turn: %+v", st.Session)
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

// TestCodexRequestedInputOverHTTP: requested input is durable, blocks
// runtime sleep, and is answered by exact input ID.
func TestCodexRequestedInputOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "ask")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "ask", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "ask", api.EventInputRequested)
	var req struct {
		InputID   string `json:"inputId"`
		TurnID    string `json:"turnId"`
		ItemID    string `json:"itemId"`
		Questions []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	last := page.Records[len(page.Records)-1]
	if err := json.Unmarshal(last.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.InputID == "" || req.TurnID == "" || len(req.Questions) != 1 ||
		req.Questions[0].ID != "q1" || len(req.Questions[0].Options) != 2 {
		t.Fatalf("input payload = %+v", req)
	}

	// Waiting input is a hard sleep blocker and is visible on the wire.
	st, err := c.Status(ctx, "ask")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateWaitingInput || st.Session.Activity != "waiting_input" {
		t.Fatalf("status = %+v", st.Session)
	}
	// A pending input request stays waiting_input: the turn submission
	// writes "active" before submitting, never after.
	time.Sleep(200 * time.Millisecond)
	st, err = c.Status(ctx, "ask")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateWaitingInput {
		t.Fatalf("state regressed to %s while input is pending", st.Session.State)
	}
	if ok, why := d.supervisor.CanSleep(codex.RuntimeKey); ok {
		t.Fatalf("runtime must not be sleepable with pending input (%s)", why)
	}
	// The HTTP surface refuses an unknown input ID.
	if _, err := c.AnswerInput(ctx, "ask", "in_nope",
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err == nil {
		t.Fatal("unknown input id must fail")
	}

	if _, err := c.AnswerInput(ctx, "ask", req.InputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err != nil {
		t.Fatalf("answer input: %v", err)
	}
	waitDurableTypes(t, c, "ask", api.EventMessageAgentCompleted)
	st, err = c.Status(ctx, "ask")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateIdle {
		t.Fatalf("state after answer = %s", st.Session.State)
	}
	if ok, why := d.supervisor.CanSleep(codex.RuntimeKey); !ok {
		t.Fatalf("runtime must be sleepable after the answer: %s", why)
	}
}

// TestCodexCancelOverHTTP: cancel is exact and the harness reports the
// terminal state.
func TestCodexCancelOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	_, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "stop")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "stop", "hold", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "stop", api.EventInputRequested)
	if _, err := c.Cancel(ctx, "stop"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDurableTypes(t, c, "stop", api.EventTurnInterrupted)
	// No turn in flight now: a second cancel is a stable conflict.
	if _, err := c.Cancel(ctx, "stop"); err == nil {
		t.Fatal("cancel without an active turn must fail")
	} else {
		wantAPIErr(t, err, api.ErrNoActiveTurn)
	}
}

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

// TestCodexConfigOverHTTP: model/mode changes are accepted durably and are
// applied to the next native thread start.
func TestCodexConfigOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	_, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "cfg")

	ctx, cancel := tctx(t)
	defer cancel()
	model, mode := "gpt-5-codex", "fast"
	resp, err := c.SetConfig(ctx, "cfg", &model, &mode)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if resp.Session.Model != model || resp.Session.Mode != mode {
		t.Fatalf("config not accepted: %+v", resp.Session)
	}
	if _, err := c.SetConfig(ctx, "cfg", nil, nil); err == nil {
		t.Fatal("empty config must be rejected")
	}
	if _, err := c.Prompt(ctx, "cfg", "hello", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "cfg", api.EventMessageAgentCompleted)
	for _, r := range page.Records {
		if r.Type != api.EventHarnessStarted {
			continue
		}
		var started struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.Model != model {
			t.Fatalf("thread started with model %q, want %q", started.Model, model)
		}
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

// TestCodexRuntimeDeathAndRecoveryOverHTTP: a runtime crash fails the
// in-flight turn durably, drops the session COLD, and the next prompt
// resumes the exact native session (when one was materialized).
func TestCodexRuntimeDeathAndRecoveryOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "crash")
	ctx, cancel := tctx(t)
	defer cancel()

	if _, err := c.Prompt(ctx, "crash", "first", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "crash", api.EventMessageAgentCompleted)
	st, err := c.Status(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	native := st.Session.NativeSessionID
	if native == "" {
		t.Fatal("session not materialized")
	}

	// The next generation's app-server dies with a turn in flight. The
	// submission may be accepted before the crash (the harness answered
	// first), so the contract under test is the durable failure record,
	// not the HTTP status.
	t.Setenv("FAKE_CODEX_MODE", "die-on-turn")
	if err := d.supervisor.Stop(codex.RuntimeKey); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	if d.supervisor.Len() != 0 {
		t.Fatal("runtime must be gone")
	}
	_, _ = c.Prompt(ctx, "crash", "second", "", "")
	waitDurableTypes(t, c, "crash", api.EventRuntimeExited)
	waitDurableTypes(t, c, "crash", api.EventTurnFailed)
	if d.supervisor.Len() != 0 {
		t.Fatalf("dead runtime still registered: %d", d.supervisor.Len())
	}

	// Recovery: a fresh generation resumes the exact same native thread.
	t.Setenv("FAKE_CODEX_MODE", "happy")
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := c.Prompt(ctx, "crash", "third", "", "")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery prompt kept failing: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitDurableTypes(t, c, "crash", api.EventMessageAgentCompleted)
	page, err := c.Transcript(ctx, "crash", 200)
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, r := range page.Records {
		if r.Type != api.EventHarnessStarted {
			continue
		}
		var started struct {
			NativeThreadID string `json:"nativeThreadId"`
			Resumed        bool   `json:"resumed"`
		}
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.NativeThreadID != native {
			t.Fatalf("thread %q, want the exact %q", started.NativeThreadID, native)
		}
		if started.Resumed {
			resumed++
		}
	}
	// Every generation after the first must reattach the exact thread.
	if resumed < 1 {
		t.Fatalf("resume records = %d, want at least 1", resumed)
	}
	st, err = c.Status(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.NativeSessionID != native || st.Session.Generation != 2 {
		t.Fatalf("identity/generation drifted: %+v", st.Session)
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
	if _, err := c.CreateCodex(ctx, "restart", t.TempDir(), "", ""); err != nil {
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
			NativeThreadID string `json:"nativeThreadId"`
			Resumed        bool   `json:"resumed"`
		}
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.Resumed {
			resumedID = started.NativeThreadID
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

// TestCodexMissingHarnessFailsClosed: creating a Codex session on a host
// without the harness fails closed with a stable code.
func TestCodexMissingHarnessFailsClosed(t *testing.T) {
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.CodexCommand = func() (codex.Command, error) {
			return codex.Command{}, os.ErrNotExist
		}
	})
	ctx, cancel := tctx(t)
	defer cancel()
	_, err := c.CreateCodex(ctx, "nope", t.TempDir(), "", "")
	if err == nil {
		t.Fatal("create must fail without the harness")
	}
	wantAPIErr(t, err, api.ErrRuntimeUnavailable)
	// The key stays free.
	if _, err := c.CreateCodex(ctx, "nope", t.TempDir(), "", ""); err == nil {
		t.Fatal("expected failure again")
	}
}

// TestCodexFixtureSessionsUnaffected: the fixture harness keeps working
// alongside the Codex adapter (no shared-state regressions).
func TestCodexFixtureSessionsUnaffected(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "native")
	if _, err := serve(t, c, "fixture-one"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "native", "hi", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "native", api.EventMessageAgentCompleted)
	if _, err := c.Prompt(ctx, "fixture-one", "hi", "", ""); err == nil {
		t.Fatal("prompt must be rejected for a fixture session")
	}
	if _, err := c.Cancel(ctx, "fixture-one"); err == nil {
		t.Fatal("cancel must be rejected for a fixture session")
	}
	if d.supervisor.Len() != 2 {
		t.Fatalf("runtimes = %d, want 2 (one per harness)", d.supervisor.Len())
	}
	// Stopping the fixture session must not touch the Codex runtime.
	if _, err := c.StopSession(ctx, "fixture-one"); err != nil {
		t.Fatal(err)
	}
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 after fixture stop", d.supervisor.Len())
	}
	if _, err := c.Prompt(ctx, "native", "still here", "", ""); err != nil {
		t.Fatalf("codex session broken by fixture stop: %v", err)
	}
	waitDurableTypes(t, c, "native", api.EventMessageAgentCompleted)
}

// codexRoot creates an isolated HOME + RepoSuite state root.
func codexRoot(t *testing.T) (home, root string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "home")
	root = filepath.Join(base, "rs")
	for _, d := range []string{home, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home, root
}

// dialClient waits for the daemon descriptor and returns a client.
func dialClient(t *testing.T, p paths.Paths) *client.Client {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon did not become reachable")
	return nil
}
