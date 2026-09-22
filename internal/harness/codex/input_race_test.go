package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// TestRespondFailTerminalRaceSettles covers the confirmed race:
//
//	AnswerInput claims responding
//	terminal path visits → wantAbort
//	Conn.Respond fails (the connection died)
//	→ the reconcile aborts durably and the projection settles.
//
// The testBeforeRespond seam makes the interleave exact: the terminal
// sweep and the connection death both run inside the Respond window.
func TestRespondFailTerminalRaceSettles(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("racefail", t.TempDir())
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "ask me"}); err != nil {
		t.Fatal(err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	var req inputRequestedPayload
	if err := json.Unmarshal(e.durablePayload(m.Session.ID, api.EventInputRequested), &req); err != nil {
		t.Fatal(err)
	}

	st := e.adapter.state(m.Session.ID)
	if st == nil {
		t.Fatal("session not tracked")
	}
	e.adapter.testBeforeRespond = func() {
		// Terminal sweep lands while Respond is in flight: it must mark
		// wantAbort and NOT publish over an owned side effect.
		e.adapter.abortInputs(st, "turn completed")
		// Then the native write fails — the connection is dead.
		e.adapter.mu.Lock()
		srv := e.adapter.servers[RuntimeKey]
		e.adapter.mu.Unlock()
		if srv != nil {
			srv.Conn().Close(errors.New("test: conn died mid-respond"))
		}
	}
	defer func() { e.adapter.testBeforeRespond = nil }()

	err := e.adapter.AnswerInput(context.Background(), m, req.InputID,
		[]harness.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
	if err == nil {
		t.Fatal("answer must fail — the native write never landed")
	}
	// The reconcile publishes input.aborted and the projection settles —
	// no restart required.
	e.waitDurable(m.Session.ID, api.EventInputAborted)
	waitFor(t, "input settled", func() bool {
		return len(e.adapter.PendingInputs(m.Session.ID)) == 0
	})
	if got := e.adapter.State(m.Session.ID); got.PendingInputs != 0 {
		t.Fatalf("pending inputs = %d after abort reconcile", got.PendingInputs)
	}
	// Not waiting_input: the turn is still tracked in-flight (the fake
	// never saw a response), so the canonical projection is active.
	if got := e.reload(m.Session.ID).State; got == session.StateWaitingInput {
		t.Fatal("session stuck waiting_input with zero pending inputs")
	}
	if p := e.durablePayload(m.Session.ID, api.EventInputResolved); p != nil {
		t.Fatal("input.resolved published though the native write failed")
	}
}

// inputCountFile polls for a fake evidence file and returns its content
// ("" when it never appeared — zero response frames is a real outcome).
func inputCountFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			return string(raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// announceBarrier arms the store hook that holds the durable
// input.requested append open, and returns the release channel.
func announceBarrier(t *testing.T, e *env) (blocked, release chan struct{}) {
	t.Helper()
	blocked, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	e.store.SetHooks(&store.Hooks{WriteFile: func(f *os.File, b []byte) (int, error) {
		if strings.HasSuffix(f.Name(), "transcript.jsonl") &&
			bytes.Contains(b, []byte(api.EventInputRequested)) {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return f.Write(b)
	}})
	return blocked, release
}

// answerableErr accepts the two legal non-answerable results a parked
// answer may see after a terminal reconciliation: the entry still
// retained but marked (BUSY) or already removed (UNKNOWN_INPUT).
func answerableErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNoSuchInput) {
		t.Fatalf("parked answer err = %v, want BUSY or UNKNOWN_INPUT", err)
	}
}

// TestParkedAnswerTerminalRace: a parked AnswerInput must never send
// Respond once terminal intent (wantAbort) exists — the announcing
// input is marked while its durable append is held, then the barrier
// opens and the parked answer races the owner's abort commit.
func TestParkedAnswerTerminalRace(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("parkrace", t.TempDir())
	respFile := filepath.Join(t.TempDir(), "resp")
	errFile := filepath.Join(t.TempDir(), "err")
	t.Setenv("FAKE_CODEX_INPUTRESP_FILE", respFile)
	t.Setenv("FAKE_CODEX_INPUTERR_FILE", errFile)

	blocked, release := announceBarrier(t, e)
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "ask me"}); err != nil {
		t.Fatal(err)
	}
	<-blocked // announcement holds its durable append open
	ids := e.adapter.PendingInputs(m.Session.ID)
	if len(ids) != 1 {
		t.Fatalf("pending inputs = %v, want the announcing entry", ids)
	}
	// The answer parks on the not-yet-closed ready barrier.
	answerErr := make(chan error, 1)
	go func() {
		answerErr <- e.adapter.AnswerInput(context.Background(), m, ids[0],
			[]harness.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
	}()
	// Terminal intent lands while the announcement is still blocked.
	st := e.adapter.state(m.Session.ID)
	e.adapter.abortInputs(st, "turn completed")
	close(release)

	if err := <-answerErr; err != nil {
		answerableErr(t, err)
	}
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	e.waitDurable(m.Session.ID, api.EventInputAborted)
	types := e.durableTypes(m.Session.ID)
	reqAt, abAt := -1, -1
	for i, typ := range types {
		switch typ {
		case api.EventInputRequested:
			reqAt = i
		case api.EventInputAborted:
			abAt = i
		case api.EventInputResolved:
			t.Fatal("input.resolved must never exist for an aborted input")
		}
	}
	if reqAt < 0 || abAt < reqAt {
		t.Fatalf("durable order = %v, want requested before aborted", types)
	}
	if got := inputCountFile(t, respFile); got != "1" {
		t.Fatalf("total native response frames = %q, want 1", got)
	}
	if got := inputCountFile(t, errFile); got != "1" {
		t.Fatalf("native ERROR response frames = %q, want 1 (the handler rejection)", got)
	}
	waitFor(t, "no pending inputs", func() bool {
		return len(e.adapter.PendingInputs(m.Session.ID)) == 0
	})
}

// TestParkedAnswerAbortAppendFailure: terminal intent plus a FAILED
// input.aborted append leaves pending+wantAbort — retained, still
// non-answerable, retryable by the next sweep.
func TestParkedAnswerAbortAppendFailure(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("parkfail", t.TempDir())

	inflight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var released atomic.Bool
	var failAbort atomic.Bool
	failAbort.Store(true)
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	e.store.SetHooks(&store.Hooks{WriteFile: func(f *os.File, b []byte) (int, error) {
		if !strings.HasSuffix(f.Name(), "transcript.jsonl") {
			return f.Write(b)
		}
		if bytes.Contains(b, []byte(api.EventInputRequested)) {
			once.Do(func() {
				close(inflight)
				<-release
			})
		}
		if failAbort.Load() && bytes.Contains(b, []byte(api.EventInputAborted)) {
			return 0, errors.New("injected aborted-append failure")
		}
		return f.Write(b)
	}})

	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "ask me"}); err != nil {
		t.Fatal(err)
	}
	<-inflight // announcement holds its durable append open
	ids := e.adapter.PendingInputs(m.Session.ID)
	if len(ids) != 1 {
		t.Fatalf("pending inputs = %v, want the announcing entry", ids)
	}
	answerErr := make(chan error, 1)
	go func() {
		answerErr <- e.adapter.AnswerInput(context.Background(), m, ids[0],
			[]harness.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
	}()
	st := e.adapter.state(m.Session.ID)
	e.adapter.abortInputs(st, "turn completed")
	if released.CompareAndSwap(false, true) {
		close(release)
	}

	if err := <-answerErr; err != nil {
		answerableErr(t, err)
	}
	// The requested committed; the abort failed — pending authority is
	// retained, wantAbort still set, session still waiting_input.
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	waitFor(t, "retained pending input", func() bool {
		return len(e.adapter.PendingInputs(m.Session.ID)) == 1
	})
	if got := e.reload(m.Session.ID).State; got != session.StateWaitingInput {
		t.Fatalf("state = %s, want waiting_input for retained authority", got)
	}
	types := e.durableTypes(m.Session.ID)
	for _, typ := range types {
		if typ == api.EventInputAborted || typ == api.EventInputResolved {
			t.Fatalf("unexpected terminal record: %v", types)
		}
	}
	// A later sweep commits the retained abort — never an answer.
	failAbort.Store(false)
	e.adapter.abortInputs(st, "turn completed")
	e.waitDurable(m.Session.ID, api.EventInputAborted)
	waitFor(t, "no pending inputs", func() bool {
		return len(e.adapter.PendingInputs(m.Session.ID)) == 0
	})
}

// TestParkedAnswerContextCancel: a caller parked on the ready barrier
// leaves promptly when its context is canceled — the announcement
// itself stays owned by its native handler.
func TestParkedAnswerContextCancel(t *testing.T) {
	e := newEnv(t, "input")
	m := e.newSession("parkctx", t.TempDir())
	respFile := filepath.Join(t.TempDir(), "resp")
	t.Setenv("FAKE_CODEX_INPUTRESP_FILE", respFile)

	blocked, release := announceBarrier(t, e)
	if _, err := e.adapter.Prompt(context.Background(), m, harness.PromptCommand{Text: "ask me"}); err != nil {
		t.Fatal(err)
	}
	<-blocked
	ids := e.adapter.PendingInputs(m.Session.ID)
	if len(ids) != 1 {
		t.Fatalf("pending inputs = %v, want the announcing entry", ids)
	}
	ctx, cancel := context.WithCancel(context.Background())
	answerErr := make(chan error, 1)
	go func() {
		answerErr <- e.adapter.AnswerInput(ctx, m, ids[0],
			[]harness.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}})
	}()
	cancel()
	select {
	case err := <-answerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parked answer did not leave on cancel")
	}
	// The announcement still completes under its original owner — and a
	// deferred (ErrPendingResponse) request produces NO response frame,
	// so the evidence file never appears.
	close(release)
	e.waitDurable(m.Session.ID, api.EventInputRequested)
	if _, err := os.Stat(respFile); !os.IsNotExist(err) {
		t.Fatalf("response count file exists (%v) — a native frame was sent", err)
	}
}
