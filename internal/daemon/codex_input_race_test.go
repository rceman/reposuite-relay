package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestCodexConcurrentAnswerSingleResponse proves the per-input claim:
// N concurrent answer attempts produce exactly one native response and
// exactly one durable input.resolved; the losers converge on BUSY or
// UNKNOWN_INPUT.
func TestCodexConcurrentAnswerSingleResponse(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	respFile := filepath.Join(t.TempDir(), "resp")
	t.Setenv("FAKE_CODEX_INPUTRESP_FILE", respFile)
	_, p, c, _ := startInProcess(t, codexOptions)
	sess := serveCodex(t, c, "conc")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "conc", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "conc", api.EventInputRequested)
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

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = c.AnswerInput(ctx, "conc", inputID,
				[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("unexpected error shape: %v", err)
		}
		if apiErr.Code != api.ErrSessionBusy && apiErr.Code != api.ErrUnknownInput {
			t.Fatalf("concurrent answer error = %s, want BUSY or UNKNOWN_INPUT", apiErr.Code)
		}
	}
	if ok != 1 {
		t.Fatalf("successful answers = %d, want exactly 1", ok)
	}
	if got := inputRespCount(t, respFile); got != "1" {
		t.Fatalf("native response count = %s, want exactly 1", got)
	}
	waitDurableTypes(t, c, "conc", api.EventMessageAgentCompleted)
	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputResolved); n != 1 {
		t.Fatalf("input.resolved count = %d, want exactly 1", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0", n)
	}
}

// TestCodexInputCommitTurnCompletionRace holds the durable
// input.resolved commit open while the fake immediately finishes the
// turn — turn/completed and its terminal sweep queue behind the in-flight
// commit. The outcome must be exactly one input.resolved and zero
// input.aborted, and the resolution commits before the turn's terminal
// record can land.
func TestCodexInputCommitTurnCompletionRace(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")

	var once sync.Once
	var released atomic.Bool
	blocked := make(chan struct{})
	release := make(chan struct{})
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	// Even on failure the gate opens, so a blocked commit never wedges
	// the daemon's shutdown drain.
	t.Cleanup(unblock)
	_, p, c, _ := startInProcess(t, func(o *Options) {
		codexOptions(o)
		o.StoreHooks = transcriptFault(func(b []byte) error {
			if bytes.Contains(b, []byte(api.EventInputResolved)) {
				once.Do(func() {
					close(blocked)
					<-release
				})
			}
			return nil
		})
	})
	sess := serveCodex(t, c, "race")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "race", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	inputID := requestedInputID(t, c, "race")

	done := make(chan error, 1)
	go func() {
		_, err := c.AnswerInput(ctx, "race", inputID,
			[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
		done <- err
	}()

	// Respond already happened (the fake finished the turn the instant it
	// received the response); the durable commit is held open. While it
	// is, the turn's terminal durable record cannot land — the session's
	// durable stream is serialized behind the in-flight commit.
	<-blocked
	pollFor(t, "turn terminal record pending behind the commit", func() bool {
		raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
		return err == nil && !bytes.Contains(raw, []byte(api.EventMessageAgentCompleted))
	})
	unblock()
	if err := <-done; err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitSessionState(t, c, "race", session.StateIdle)

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
		t.Fatalf("input.aborted count = %d, want 0 — resolved wins the race", n)
	}
	// The terminal record may legally land before the commit (the
	// notification goroutine can win the session's publish mutex while
	// the commit is held): the exclusivity invariant is that exactly one
	// terminal input outcome exists — input.resolved, never
	// input.aborted over a native-answered input.
}
