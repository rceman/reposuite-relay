package daemon

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/session"
	"testing"
	"time"
)

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
