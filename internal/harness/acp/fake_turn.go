package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// onSessionPrompt runs one scripted turn. The prompt RESPONSE is the terminal
// turn result, so the handler defers and a turn goroutine answers later: the
// reader loop must stay free to receive cancel notifications and permission
// responses.
func (a *fakeAgent) onSessionPrompt(_ context.Context, id int64, params json.RawMessage) (any, error) {
	var p PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad session/prompt params: %w", err)
	}
	if _, ok := a.store.load(p.SessionID); !ok {
		return nil, fmt.Errorf("session not found: %s", p.SessionID)
	}
	text := promptText(p)
	a.mu.Lock()
	a.turnSeq++
	turn := a.turnSeq
	a.mu.Unlock()
	a.events.record("session/prompt", p.SessionID)

	switch a.cfg.Mode {
	case FakeDieOnPrompt:
		os.Exit(3)
	case FakeMalformed:
		fmt.Fprint(a.raw, "this is not json-rpc\n")
		time.Sleep(time.Hour)
	case FakeOversized:
		fmt.Fprint(a.raw, strings.Repeat("x", MaxFrameBytes+16)+"\n")
		time.Sleep(time.Hour)
	case FakeUnknownRequest:
		go a.runUnknownRequestTurn(id, p, text)
	default:
		go a.runTurn(id, turn, p, text)
	}
	return nil, ErrPendingResponse
}

// runTurn streams a scripted turn and answers the deferred prompt request.
func (a *fakeAgent) runTurn(id int64, turn int, p PromptParams, text string) {
	// A cancel that arrives before the turn starts still wins.
	cancelled := false
	select {
	case <-a.cancelSignal(p.SessionID):
		cancelled = true
	default:
	}

	switch a.cfg.Mode {
	case FakeCancel:
		a.emit(p.SessionID, Update{
			SessionUpdate: UpdateAgentMessageChunk,
			Content:       content("partial: " + text),
		})
		<-a.cancelSignal(p.SessionID)
		a.finish(id, p.SessionID, StopCancelled, nil)
		return
	case FakePermission, FakePermissionUnknown:
		if !a.runPermissionRoundTrip(id, p, text) {
			return
		}
		return
	case FakeDieOnPermission:
		go a.requestPermissionThenDie(p.SessionID)
		return
	}

	if a.cfg.Mode == FakeThought {
		a.emit(p.SessionID, Update{
			SessionUpdate: UpdateAgentThoughtChunk,
			Content:       content("hidden reasoning about " + text),
		})
	}
	if cancelled {
		a.finish(id, p.SessionID, StopCancelled, nil)
		return
	}
	if a.cfg.Mode == FakeFailTurn {
		// A refused turn: the turn fails, the runtime stays alive.
		a.finish(id, p.SessionID, "refusal", nil)
		return
	}
	if a.cfg.Mode == FakeTools {
		a.emitToolCalls(p.SessionID)
	}
	reply := "echo: " + text
	for _, part := range chunks(reply, 3) {
		a.emit(p.SessionID, Update{
			SessionUpdate: UpdateAgentMessageChunk,
			Content:       content(part),
		})
	}
	a.emit(p.SessionID, Update{
		SessionUpdate: UpdateUsage,
		Size:          int64p(200000),
		Used:          int64p(1234),
	})
	_ = turn
	a.finish(id, p.SessionID, StopEndTurn, &Usage{
		InputTokens:   int64p(10),
		OutputTokens:  int64p(5),
		TotalTokens:   int64p(15),
		ThoughtTokens: int64p(2),
	})
}

// finish records the completed turn durably and answers the prompt request.
func (a *fakeAgent) finish(id int64, sessionID, stopReason string, usage *Usage) {
	if stopReason == StopEndTurn {
		_ = a.store.appendTurn(sessionID, "turn")
	}
	a.events.record("turn-complete", stopReason)
	_ = a.conn.Respond(id, PromptResult{
		StopReason: stopReason,
		Usage:      usage,
	})
}

// runPermissionRoundTrip asks the client for approval, then completes. It
// returns false when the round trip ended the process.
func (a *fakeAgent) runPermissionRoundTrip(id int64, p PromptParams, text string) bool {
	options := []PermissionOption{
		{Kind: KindAllowOnce, Name: "Allow once", OptionID: "once"},
		{Kind: KindAllowAlways, Name: "Always allow", OptionID: "always"},
		{Kind: KindRejectOnce, Name: "Reject", OptionID: "reject"},
	}
	if a.cfg.Mode == FakePermissionUnknown {
		// No recognizable allow option: a correct client fails closed.
		options = []PermissionOption{
			{Kind: "escalate", Name: "Escalate to a human", OptionID: "escalate-42"},
		}
	}
	call, err := a.conn.StartCall(MethodRequestPermission, RequestPermissionParams{
		Options:   options,
		SessionID: p.SessionID,
		ToolCall:  json.RawMessage(`{"toolCallId":"t1","title":"write file"}`),
	})
	if err != nil {
		a.finish(id, p.SessionID, StopRefusal, nil)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res RequestPermissionResult
	if err := call.Wait(ctx, &res); err != nil {
		a.finish(id, p.SessionID, StopRefusal, nil)
		return false
	}
	a.events.record("permission", res.Outcome.Outcome+":"+res.Outcome.OptionID)
	a.emit(p.SessionID, Update{
		SessionUpdate: UpdateAgentMessageChunk,
		Content:       content("echo: " + text + " [" + res.Outcome.Outcome + ":" + res.Outcome.OptionID + "]"),
	})
	a.finish(id, p.SessionID, StopEndTurn, &Usage{
		InputTokens:  int64p(3),
		OutputTokens: int64p(2),
		TotalTokens:  int64p(5),
	})
	return true
}

// requestPermissionThenDie exits while an approval request is in flight.
func (a *fakeAgent) requestPermissionThenDie(sessionID string) {
	_, err := a.conn.StartCall(MethodRequestPermission, RequestPermissionParams{
		Options: []PermissionOption{
			{Kind: KindAllowOnce, Name: "Allow once", OptionID: "once"},
		},
		SessionID: sessionID,
	})
	if err != nil {
		os.Exit(3)
	}
	time.Sleep(2 * time.Second)
	os.Exit(3)
}

// runUnknownRequestTurn sends an agent→client request Relay does not
// implement; a correct client answers with an explicit error, which the fake
// then reports back in the turn text.
func (a *fakeAgent) runUnknownRequestTurn(id int64, p PromptParams, text string) {
	call, err := a.conn.StartCall("fs/read_text_file", map[string]any{"sessionId": p.SessionID, "path": "/etc/hostname"})
	if err != nil {
		a.finish(id, p.SessionID, StopRefusal, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out json.RawMessage
	waitErr := call.Wait(ctx, &out)
	detail := "answered"
	if waitErr != nil {
		detail = waitErr.Error()
	}
	a.events.record("unknown-request", detail)
	a.emit(p.SessionID, Update{
		SessionUpdate: UpdateAgentMessageChunk,
		Content:       content("echo: " + text),
	})
	a.finish(id, p.SessionID, StopEndTurn, nil)
}

// emit sends one session/update notification.
func (a *fakeAgent) emit(sessionID string, update Update) {
	_ = a.conn.Notify(MethodSessionUpdate, SessionUpdateNotification{
		SessionID: sessionID,
		Update:    update,
	})
}

// spawnChild starts a long-lived child so process-tree teardown is provable.
func (a *fakeAgent) spawnChild() {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		return
	}
	a.events.record("child", fmt.Sprint(cmd.Process.Pid))
	go func() { _ = cmd.Wait() }()
}

// ignoreStop makes the graceful phase ineffective, forcing the bounded
// SIGTERM/SIGKILL escalation.
func (a *fakeAgent) ignoreStop() {
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
}

// configOptions is the advertised configuration surface per vendor. OpenCode
// advertises model/effort/mode select options; Devin advertises mode only —
// its model is a process-level choice, which is exactly why Relay partitions
// Devin runtimes by model.
func (a *fakeAgent) configOptions(rec fakeSessionRecord) []ConfigOption {
	model := rec.Model
	if model == "" {
		model = "fake/model-a"
	}
	mode := rec.Mode
	if mode == "" {
		mode = a.defaultMode()
	}
	out := []ConfigOption{}
	if a.cfg.Vendor == FakeVendorOpenCode {
		out = append(out,
			ConfigOption{
				ID:           "model",
				Name:         "Model",
				Category:     CategoryModel,
				Type:         TypeSelect,
				CurrentValue: rawString(model),
				Options: mustRaw([]ConfigSelectOption{
					{Value: "fake/model-a", Name: "Fake/Model A"},
					{Value: "fake/model-b", Name: "Fake/Model B"},
				}),
			},
			ConfigOption{
				ID:           "effort",
				Name:         "Effort",
				Category:     CategoryThoughtLevel,
				Type:         TypeSelect,
				CurrentValue: rawString("medium"),
				Options: mustRaw([]ConfigSelectOption{
					{Value: "low", Name: "Low"},
					{Value: "medium", Name: "Medium"},
				}),
			})
	}
	out = append(out, ConfigOption{
		ID:           "mode",
		Name:         "Session Mode",
		Category:     CategoryMode,
		Type:         TypeSelect,
		CurrentValue: rawString(mode),
		Options: mustRaw([]ConfigSelectOption{
			{Value: "ask", Name: "Ask"},
			{Value: "smart", Name: "Smart"},
			{Value: "accept-edits", Name: "Accept edits"},
			{Value: "bypass", Name: "Bypass permissions"},
		}),
	})
	return out
}

// defaultMode is the vendor default mode the fake starts sessions in. Devin
// starts in "ask" (so Relay must explicitly select "bypass" for full
// permissions); OpenCode starts in "smart".
func (a *fakeAgent) defaultMode() string {
	if a.cfg.Vendor == FakeVendorDevin {
		return "ask"
	}
	return "smart"
}

// modeAvailable reports whether a mode ID is advertised.
func (a *fakeAgent) modeAvailable(id string) bool {
	for _, o := range a.configOptions(fakeSessionRecord{}) {
		if o.Category != CategoryMode {
			continue
		}
		return optionHasValue(o, id)
	}
	return false
}

func promptText(p PromptParams) string {
	var sb strings.Builder
	for _, block := range p.Prompt {
		sb.WriteString(block.Text)
	}
	return sb.String()
}

func content(text string) json.RawMessage {
	block := TextBlock(text)
	b, _ := json.Marshal(block)
	return b
}

func chunks(s string, n int) []string {
	if n <= 1 || len(s) <= n {
		return []string{s}
	}
	size := (len(s) + n - 1) / n
	out := []string{}
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func int64p(v int64) *int64 { return &v }

func rawString(v string) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
