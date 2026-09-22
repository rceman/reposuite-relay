package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestCodexInputResolvedPublishGap is the WEBCTRL02 commit-point audit
// probe for Adapter.AnswerInput, whose ordering is:
//
//	native Respond(...)            — external side effect
//	delete st.inputs[inputID]      — pending consumed in memory
//	publishDurable input.resolved  — durable commit
//
// If the durable publish fails after the native response was accepted,
// this test proves the exact outcome: the answer cannot be re-submitted
// (fail-closed, UNKNOWN_INPUT — never a double-answer), the turn
// proceeds, and the durable input.requested is left UNRESOLVED — no
// input.resolved, and no compensating input.aborted, because the pending
// entry was already dropped before the failed publish. The stale
// requested input therefore remains in the durable transcript forever.
//
// The test asserts this observed behavior verbatim so the gap is
// reproducible; the proposed correction (delete the pending entry only
// after a successful publish, letting turn-end/runtime-exit abortInputs
// reconcile durably) is reported with the audit, not applied here.
func TestCodexInputResolvedPublishGap(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	_, p, c, _ := startInProcess(t, codexOptions)
	sess := serveCodex(t, c, "gap")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "gap", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "gap", api.EventInputRequested)
	var req struct {
		InputID string `json:"inputId"`
	}
	for _, r := range page.Records {
		if r.Type != api.EventInputRequested {
			continue
		}
		if err := json.Unmarshal(r.Payload, &req); err != nil {
			t.Fatal(err)
		}
	}
	if req.InputID == "" {
		t.Fatal("no input.requested record")
	}

	// Fault injection: the transcript file becomes unwritable, so the
	// durable input.resolved append fails AFTER the native request has
	// already been answered. Everything else (session.json, the fake
	// harness) keeps working.
	trPath := filepath.Join(p.SessionsDir(), sess.SessionID, "transcript.jsonl")
	if err := os.Chmod(trPath, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err := c.AnswerInput(ctx, "gap", req.InputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
	if err := os.Chmod(trPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("expected the durable publish to fail")
	}

	// Fail-closed: the pending entry is consumed — a retry is a clean
	// UNKNOWN_INPUT, never a second native response.
	if _, err := c.AnswerInput(ctx, "gap", req.InputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err == nil {
		t.Fatal("re-answering a consumed input must fail")
	} else {
		wantAPIErr(t, err, api.ErrUnknownInput)
	}

	// The turn completes natively (the harness already has the answer).
	// Wait for the terminal bookkeeping — session state returns to idle —
	// then inspect the durable transcript: the requested input must be
	// reconciled there. IT IS NOT: this is the audited gap.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, serr := c.Status(ctx, "gap")
		if serr == nil && st.Session.State == session.StateIdle {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := os.ReadFile(trPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(api.EventInputRequested)) {
		t.Fatal("input.requested missing from durable transcript")
	}
	if bytes.Contains(raw, []byte(api.EventInputResolved)) {
		t.Fatal("input.resolved present despite the failed durable publish")
	}
	if bytes.Contains(raw, []byte(api.EventInputAborted)) {
		t.Fatal("input.aborted present — pending entry should have been consumed")
	}
}
