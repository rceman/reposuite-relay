package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

var errInjectedWrite = errors.New("injected transcript write failure")

// transcriptFault routes every transcript.jsonl record append through
// fn — returning an error injects a durable-append failure; blocking
// inside fn holds a commit window open for deterministic race tests.
func transcriptFault(fn func(b []byte) error) *store.Hooks {
	return &store.Hooks{
		WriteFile: func(f *os.File, b []byte) (int, error) {
			if strings.HasSuffix(f.Name(), "transcript.jsonl") && fn != nil {
				if err := fn(b); err != nil {
					return 0, err
				}
			}
			return f.Write(b)
		},
	}
}

func transcriptPath(t *testing.T, p paths.Paths, sessionID string) string {
	t.Helper()
	return filepath.Join(p.SessionsDir(), sessionID, "transcript.jsonl")
}

// countType counts durable records of one type in transcript bytes.
func countType(t *testing.T, raw []byte, typ string) int {
	t.Helper()
	return bytes.Count(raw, []byte(`"type":"`+typ+`"`))
}

// lastPayload decodes the payload of the last record of a type.
func lastPayload(t *testing.T, raw []byte, typ string) json.RawMessage {
	t.Helper()
	var found json.RawMessage
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"type":"`+typ+`"`)) {
			continue
		}
		var r struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("malformed record: %v", err)
		}
		found = r.Payload
	}
	return found
}

// inputRespCount reads the fake's at-most-once evidence file. The fake
// writes it asynchronously after receiving a response, so the read polls
// until the file exists.
func inputRespCount(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			return string(raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("input-response count file %s never appeared", path)
	return ""
}

// waitSessionState polls until the session reports the given state.
func waitSessionState(t *testing.T, c *client.Client, key, want string) {
	t.Helper()
	ctx, cancel := tctx(t)
	defer cancel()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := c.Status(ctx, key)
		if err == nil && st.Session.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never reached state %s", key, want)
}

// pollFor polls a condition until the deadline.
func pollFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCodexInputCommitRetryAfterPublishFailure is the regression for the
// audited INPUT_RESOLUTION_COMMIT_GAP: a durable input.resolved append
// that fails AFTER the native Respond succeeds must not lose the input.
// The turn's terminal sweep also fails the commit; the session stays
// waiting_input (visibly non-clean); a later retry commits the retained
// sanitized payload WITHOUT a second native response, and a retry
// carrying different answers cannot rewrite native-accepted truth.
func TestCodexInputCommitRetryAfterPublishFailure(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	respFile := filepath.Join(t.TempDir(), "resp")
	t.Setenv("FAKE_CODEX_INPUTRESP_FILE", respFile)

	var failResolved atomic.Bool
	failResolved.Store(true)
	_, p, c, _ := startInProcess(t, func(o *Options) {
		codexOptions(o)
		o.StoreHooks = transcriptFault(func(b []byte) error {
			if failResolved.Load() && bytes.Contains(b, []byte(api.EventInputResolved)) {
				return errInjectedWrite
			}
			return nil
		})
	})
	sess := serveCodex(t, c, "gap")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "gap", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "gap", api.EventInputRequested)
	var inputID string
	for _, r := range page.Records {
		if r.Type == api.EventInputRequested {
			var req struct {
				InputID string `json:"inputId"`
			}
			if err := json.Unmarshal(r.Payload, &req); err != nil {
				t.Fatal(err)
			}
			inputID = req.InputID
		}
	}
	if inputID == "" {
		t.Fatal("no input.requested record")
	}

	// The answer crosses the native commit point; the durable commit
	// fails.
	if _, err := c.AnswerInput(ctx, "gap", inputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err == nil {
		t.Fatal("expected the durable publish to fail")
	}
	// Exactly one native response was sent.
	if got := inputRespCount(t, respFile); got != "1" {
		t.Fatalf("native response count = %s, want 1", got)
	}
	// The turn completes; its terminal sweep attempts the retained
	// resolution commit — which fails too — so the session must stay
	// visibly non-clean: waiting_input, pending entry retained.
	waitDurableTypes(t, c, "gap", api.EventMessageAgentCompleted)
	waitSessionState(t, c, "gap", session.StateWaitingInput)

	// Storage recovers: a retry commits the stored sanitized payload —
	// no second Respond, and the retry's different answers cannot
	// replace the native-accepted resolution.
	failResolved.Store(false)
	if _, err := c.AnswerInput(ctx, "gap", inputID,
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"beta"}}}); err != nil {
		t.Fatalf("commit retry: %v", err)
	}
	if got := inputRespCount(t, respFile); got != "1" {
		t.Fatalf("retry re-sent the native response (count = %s)", got)
	}
	waitSessionState(t, c, "gap", session.StateIdle)

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputRequested); n != 1 {
		t.Fatalf("input.requested count = %d, want 1", n)
	}
	if n := countType(t, raw, api.EventInputResolved); n != 1 {
		t.Fatalf("input.resolved count = %d, want exactly 1", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0 (resolved is exclusive)", n)
	}
	var res struct {
		Answers map[string][]string `json:"answers"`
	}
	if err := json.Unmarshal(lastPayload(t, raw, api.EventInputResolved), &res); err != nil {
		t.Fatal(err)
	}
	if got := res.Answers["q1"]; len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("resolved answers = %v, want the native-accepted alpha", got)
	}
}
