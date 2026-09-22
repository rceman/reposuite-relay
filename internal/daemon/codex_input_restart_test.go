package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
)

// startDaemonGeneration brings up one daemon generation on existing
// paths — the restart-test primitive. The returned cleanup drains the
// generation fully (Shutdown + Serve drain) before returning.
func startDaemonGeneration(t *testing.T, p paths.Paths, opts Options) (*Daemon, *client.Client, func()) {
	t.Helper()
	d, err := Start(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	c := dialClient(t, p)
	return d, c, func() {
		d.Shutdown()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Error("daemon generation did not shut down")
		}
	}
}

// requestedInputID extracts the durable input.requested ID.
func requestedInputID(t *testing.T, c *client.Client, key string) string {
	t.Helper()
	page := waitDurableTypes(t, c, key, api.EventInputRequested)
	for _, r := range page.Records {
		if r.Type != api.EventInputRequested {
			continue
		}
		var req struct {
			InputID string `json:"inputId"`
		}
		if err := json.Unmarshal(r.Payload, &req); err != nil {
			t.Fatal(err)
		}
		return req.InputID
	}
	t.Fatal("no input.requested record")
	return ""
}

// TestCodexRestartReconcilesUnresolvedInput: a requested input left
// unanswered across a daemon restart can never be answered again — the
// native RPC died with the generation. Startup reconciliation commits a
// durable input.aborted and normalizes the restored waiting_input state;
// a sibling session on the same shared runtime is untouched.
func TestCodexRestartReconcilesUnresolvedInput(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)

	_, c, stop := startDaemonGeneration(t, p, opts)
	sess := serveCodex(t, c, "orphan")
	bystander := serveCodex(t, c, "bystander") // sibling, no input — must stay untouched
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "orphan", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	inputID := requestedInputID(t, c, "orphan")
	waitSessionState(t, c, "orphan", session.StateWaitingInput)
	stop()

	// Generation 2: startup reconciliation must commit input.aborted for
	// the impossible pending request and restore the session COLD/idle.
	_, c2, stop2 := startDaemonGeneration(t, p, opts)
	defer stop2()

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 1 {
		t.Fatalf("input.aborted count = %d, want exactly 1", n)
	}
	var ab struct {
		InputID string `json:"inputId"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(lastPayload(t, raw, api.EventInputAborted), &ab); err != nil {
		t.Fatal(err)
	}
	if ab.InputID != inputID || ab.Reason != restartInputAbortReason {
		t.Fatalf("reconciled abort = %+v, want input %s with restart reason", ab, inputID)
	}
	st, err := c2.Status(ctx, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateIdle || st.Session.RuntimeState != session.RuntimeCold || st.Session.PID != 0 {
		t.Fatalf("restored session = %+v, want idle/COLD/pid 0", st.Session)
	}
	// The reconciled input is not answerable.
	if _, err := c2.AnswerInput(ctx, "orphan", inputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err == nil {
		t.Fatal("answering a restart-reconciled input must fail")
	} else {
		wantAPIErr(t, err, api.ErrUnknownInput)
	}
	// The sibling session's transcript is untouched by reconciliation.
	raw2, err := os.ReadFile(transcriptPath(t, p, bystander.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw2, []byte(api.EventInputAborted)) {
		t.Fatal("reconciliation leaked into the sibling session's transcript")
	}
}

// TestCodexAnsweredCommitGapRestartRecovery: a native answer that crossed
// its commit point but whose durable input.resolved never committed is,
// on restart, indistinguishable from an unanswered request — the
// canonical restart outcome is input.aborted, and the session must not
// resurrect an impossible waiting_input. The native side still saw
// exactly one response.
func TestCodexAnsweredCommitGapRestartRecovery(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	respFile := filepath.Join(t.TempDir(), "resp")
	t.Setenv("FAKE_CODEX_INPUTRESP_FILE", respFile)
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}

	var failResolved atomic.Bool
	failResolved.Store(true)
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)
	opts.StoreHooks = transcriptFault(func(b []byte) error {
		if failResolved.Load() && bytes.Contains(b, []byte(api.EventInputResolved)) {
			return errInjectedWrite
		}
		return nil
	})

	_, c, stop := startDaemonGeneration(t, p, opts)
	sess := serveCodex(t, c, "answered")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "answered", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	inputID := requestedInputID(t, c, "answered")
	if _, err := c.AnswerInput(ctx, "answered", inputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err == nil {
		t.Fatal("expected the durable publish to fail")
	}
	// The turn ends with the resolution retained but uncommitted:
	// session stays waiting_input until authority converges.
	waitDurableTypes(t, c, "answered", api.EventMessageAgentCompleted)
	waitSessionState(t, c, "answered", session.StateWaitingInput)
	stop()

	failResolved.Store(false)
	_, c2, stop2 := startDaemonGeneration(t, p, opts)
	defer stop2()

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputResolved); n != 0 {
		t.Fatalf("input.resolved count = %d, want 0 (commit never landed)", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 1 {
		t.Fatalf("input.aborted count = %d, want exactly 1 (restart reconciliation)", n)
	}
	if got := inputRespCount(t, respFile); got != "1" {
		t.Fatalf("native response count = %s, want 1 across restart", got)
	}
	st, err := c2.Status(ctx, "answered")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateIdle || st.Session.RuntimeState != session.RuntimeCold {
		t.Fatalf("restored session = %+v, want idle/COLD", st.Session)
	}
}

// TestCodexAbortPublishFailureRetained: when the durable input.aborted
// append fails during runtime death, the pending input is NOT discarded
// — it stays retained (still waiting_input) until reconciliation lands
// the terminal record exactly once.
func TestCodexAbortPublishFailureRetained(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}

	var failAborted atomic.Bool
	failAborted.Store(true)
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)
	opts.StoreHooks = transcriptFault(func(b []byte) error {
		if failAborted.Load() && bytes.Contains(b, []byte(api.EventInputAborted)) {
			return errInjectedWrite
		}
		return nil
	})

	d, c, stop := startDaemonGeneration(t, p, opts)
	sess := serveCodex(t, c, "abortfail")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "abortfail", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	requestedInputID(t, c, "abortfail")

	// Kill the runtime: the turn fails durably and the pending input's
	// input.aborted append fails — the entry must be retained.
	st, err := c.Status(ctx, "abortfail")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.PID == 0 {
		t.Fatal("no live runtime PID")
	}
	if err := syscall.Kill(st.Session.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "abortfail", api.EventRuntimeExited)
	waitSessionState(t, c, "abortfail", session.StateWaitingInput)

	// The pending authority survived in memory — not discarded.
	ad, ok := d.adapters[session.HarnessCodex]
	if !ok {
		t.Fatal("no codex adapter")
	}
	cod, ok := ad.adapter.(*codex.Adapter)
	if !ok {
		t.Fatal("adapter is not codex")
	}
	if got := cod.PendingInputs(sess.SessionID); len(got) != 1 {
		t.Fatalf("pending inputs after failed abort = %v, want 1 retained", got)
	}
	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(api.EventInputAborted)) {
		t.Fatal("input.aborted must not exist yet — the append failed")
	}
	stop()

	// Restart reconciliation commits the terminal record exactly once.
	failAborted.Store(false)
	_, _, stop2 := startDaemonGeneration(t, p, opts)
	defer stop2()
	raw, err = os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 1 {
		t.Fatalf("input.aborted count after restart = %d, want exactly 1", n)
	}
}

// TestCodexReconcileFailsClosed: when startup reconciliation cannot
// durably append input.aborted, daemon startup fails closed — no
// descriptor, no served impossible pending-input authority.
func TestCodexReconcileFailsClosed(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)

	_, c, stop := startDaemonGeneration(t, p, opts)
	serveCodex(t, c, "wedged")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "wedged", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	requestedInputID(t, c, "wedged")
	waitSessionState(t, c, "wedged", session.StateWaitingInput)
	stop()

	// Generation 2 cannot append input.aborted → Start must fail.
	opts.StoreHooks = transcriptFault(func(b []byte) error {
		if bytes.Contains(b, []byte(api.EventInputAborted)) {
			return errInjectedWrite
		}
		return nil
	})
	if _, err := Start(p, opts); err == nil {
		t.Fatal("startup must fail closed when reconciliation cannot commit")
	} else if !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("startup error = %v, want reconciliation failure", err)
	}
	if _, err := os.ReadFile(p.DaemonDescriptor()); err == nil {
		raw, _ := os.ReadFile(p.DaemonDescriptor())
		t.Fatalf("descriptor must not name a wedged generation: %s", raw)
	}
}
