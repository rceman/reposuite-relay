package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// TestCodexMultiInputStateConvergence: two concurrent requested inputs in
// one session — resolving one keeps waiting_input (the other still holds
// authority); only the final terminal outcome settles to active/idle.
func TestCodexMultiInputStateConvergence(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input-pair")
	d, p, c, _ := startInProcess(t, codexOptions)
	sess := serveCodex(t, c, "pair")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "pair", "ask me twice", "", ""); err != nil {
		t.Fatal(err)
	}
	// Both requests are durably announced.
	pollFor(t, "two input.requested records", func() bool {
		raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
		return err == nil && countType(t, raw, api.EventInputRequested) == 2
	})
	var ids []string
	page, err := c.Transcript(ctx, "pair", 100)
	if err != nil {
		t.Fatal(err)
	}
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
		ids = append(ids, req.InputID)
	}
	if len(ids) != 2 {
		t.Fatalf("input.requested IDs = %v, want 2", ids)
	}
	waitSessionState(t, c, "pair", session.StateWaitingInput)

	// Resolving the first leaves the second unresolved — the projection
	// must stay waiting_input, not drop to active.
	if _, err := c.AnswerInput(ctx, "pair", ids[0],
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"alpha"}}}); err != nil {
		t.Fatal(err)
	}
	pollFor(t, "first input resolved", func() bool {
		raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
		return err == nil && countType(t, raw, api.EventInputResolved) == 1
	})
	st, err := c.Status(ctx, "pair")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateWaitingInput {
		t.Fatalf("state = %s after resolving 1 of 2, want waiting_input", st.Session.State)
	}
	if got := codexAdapter(t, d).PendingInputs(sess.SessionID); len(got) != 1 {
		t.Fatalf("pending inputs = %v, want exactly the second", got)
	}

	// The second resolves; the turn completes; projection settles idle.
	if _, err := c.AnswerInput(ctx, "pair", ids[1],
		[]api.InputAnswer{{QuestionID: "q1", Answers: []string{"beta"}}}); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "pair", api.EventMessageAgentCompleted)
	waitSessionState(t, c, "pair", session.StateIdle)
	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputResolved); n != 2 {
		t.Fatalf("input.resolved count = %d, want 2", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0", n)
	}
}

// TestCodexInputOtherPreservesOptions: isOther offers a free-form path
// in ADDITION to structured options — the durable input.requested must
// carry both, never drop the native options.
func TestCodexInputOtherPreservesOptions(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input-other")
	_, p, c, _ := startInProcess(t, codexOptions)
	sess := serveCodex(t, c, "other")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "other", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	requestedInputID(t, c, "other")
	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Questions []struct {
			ID      string `json:"id"`
			IsOther bool   `json:"isOther"`
			Options []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(lastPayload(t, raw, api.EventInputRequested), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Questions) != 1 || !req.Questions[0].IsOther {
		t.Fatalf("input.requested questions = %+v, want one isOther question", req.Questions)
	}
	if len(req.Questions[0].Options) != 2 ||
		req.Questions[0].Options[0].Label != "alpha" || req.Questions[0].Options[1].Label != "beta" {
		t.Fatalf("options dropped by isOther: %+v", req.Questions[0].Options)
	}
}

// TestCodexReconcileMalformedAuthorityFailsClosed: a malformed input.*
// authority record in a waiting_input session's transcript is corruption
// — startup must fail closed, never normalize on a guess.
func TestCodexReconcileMalformedAuthorityFailsClosed(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)

	_, c, stop := startDaemonGeneration(t, p, opts)
	sess := serveCodex(t, c, "wedged")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "wedged", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	requestedInputID(t, c, "wedged")
	waitSessionState(t, c, "wedged", session.StateWaitingInput)
	stop()

	// Inject a malformed authority record: a valid envelope whose payload
	// is not a decodable input.* shape.
	ss, err := store.OpenSessions(p.SessionsDir(), session.NewSessionID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ss.Transcript(sess.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(store.Record{
		Version: store.RecordVersion,
		Seq:     tr.LastSeq() + 1,
		Type:    api.EventInputAborted,
		At:      time.Now().UTC(),
		Payload: json.RawMessage(`{"inputId":123}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Start(p, opts); err == nil {
		t.Fatal("startup must fail closed on malformed input.* authority")
	} else if !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("startup error = %v, want reconciliation failure", err)
	}
}
