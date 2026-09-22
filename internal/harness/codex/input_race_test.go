package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
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
