package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
)

// TestCodexSecretInputRedacted: a native isSecret question is preserved in
// the durable input.requested payload (the Web Admin needs the semantic),
// and its answer is forwarded to the harness but never persisted — the
// durable input.resolved record withholds the plaintext and marks the
// question ID as redacted instead.
func TestCodexSecretInputRedacted(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input-secret")
	_, p, c, _ := startInProcess(t, codexOptions)
	sess := serveCodex(t, c, "sec")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "sec", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	page := waitDurableTypes(t, c, "sec", api.EventInputRequested)
	var req struct {
		InputID   string `json:"inputId"`
		Questions []struct {
			ID       string `json:"id"`
			IsSecret bool   `json:"isSecret"`
			IsOther  bool   `json:"isOther"`
		} `json:"questions"`
	}
	last := page.Records[len(page.Records)-1]
	if err := json.Unmarshal(last.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Questions) != 2 || req.Questions[0].IsSecret ||
		!req.Questions[1].IsSecret || req.Questions[1].ID != "q2" {
		t.Fatalf("input.requested payload = %s", last.Payload)
	}

	const secret = "s3cr3t-pl4int3xt-n3v3r-p3rs1st"
	if _, err := c.AnswerInput(ctx, "sec", req.InputID, []api.InputAnswer{
		{QuestionID: "q1", Answers: []string{"alpha"}},
		{QuestionID: "q2", Answers: []string{secret}},
	}); err != nil {
		t.Fatalf("answer input: %v", err)
	}
	page = waitDurableTypes(t, c, "sec", api.EventInputResolved)
	var res struct {
		InputID  string              `json:"inputId"`
		Answers  map[string][]string `json:"answers"`
		Redacted []string            `json:"redacted"`
	}
	for _, r := range page.Records {
		if r.Type != api.EventInputResolved {
			continue
		}
		if err := json.Unmarshal(r.Payload, &res); err != nil {
			t.Fatal(err)
		}
	}
	if got := res.Answers["q1"]; len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("non-secret answer not preserved: %s", page.Records[len(page.Records)-1].Payload)
	}
	if _, leaked := res.Answers["q2"]; leaked {
		t.Fatal("secret answer present in durable input.resolved payload")
	}
	if len(res.Redacted) != 1 || res.Redacted[0] != "q2" {
		t.Fatalf("redacted marker = %v, want [q2]", res.Redacted)
	}
	// The turn still completes: the native harness received the secret.
	waitDurableTypes(t, c, "sec", api.EventMessageAgentCompleted)

	// Non-disclosure: the secret string is absent from the durable
	// transcript file AND from the served transcript page bytes.
	raw, err := os.ReadFile(filepath.Join(p.SessionsDir(), sess.SessionID, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("secret answer persisted in transcript.jsonl")
	}
	page, err = c.Transcript(ctx, "sec", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if bytes.Contains(r.Payload, []byte(secret)) {
			t.Fatalf("secret answer visible in served record %d", r.Seq)
		}
	}
}
