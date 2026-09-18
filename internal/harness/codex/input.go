package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// AnswerInput resolves a native requested-input request by exact Relay
// input ID. The answer is mapped back to the native question IDs and the
// original JSON-RPC request ID.
func (a *Adapter) AnswerInput(ctx context.Context, m *session.Managed, inputID string, sel []harness.InputAnswer) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	pending, ok := st.inputs[inputID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchInput, inputID)
	}
	a.mu.Unlock()

	known := map[string]bool{}
	for _, q := range pending.questionIDs {
		known[q] = true
	}
	answers := map[string]ToolRequestUserInputAnswer{}
	for _, s := range sel {
		if !known[s.QuestionID] {
			return fmt.Errorf("%w: question %s does not belong to input %s",
				ErrNoSuchInput, s.QuestionID, inputID)
		}
		answers[s.QuestionID] = ToolRequestUserInputAnswer{Answers: s.Answers}
	}
	if len(answers) == 0 {
		return fmt.Errorf("%w: no answers supplied for %s", ErrNoSuchInput, inputID)
	}

	a.mu.Lock()
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("%w: runtime gone", ErrNoSuchInput)
	}
	if err := srv.Conn().Respond(pending.nativeReqID,
		ToolRequestUserInputResponse{Answers: answers}); err != nil {
		return fmt.Errorf("respond to native input request: %w", err)
	}
	a.mu.Lock()
	delete(st.inputs, inputID)
	a.mu.Unlock()

	flat := map[string][]string{}
	for q, ans := range answers {
		flat[q] = ans.Answers
	}
	if err := a.publishDurable(m, api.EventInputResolved, inputResolvedPayload{
		InputID: inputID,
		Answers: flat,
	}); err != nil {
		return err
	}
	// The turn continues: it is active again, not waiting for input.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	return nil
}

// PendingInputs returns the unresolved input IDs for a session.
func (a *Adapter) PendingInputs(sessionID string) []string {
	st := a.state(sessionID)
	if st == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(st.inputs))
	for id := range st.inputs {
		out = append(out, id)
	}
	return out
}

// --- server→client requests ----------------------------------------

// handleRequestUserInput defers the native response: Relay publishes a
// durable input.requested event, blocks runtime sleep, and answers only
// when a user supplies input.
func (a *Adapter) handleRequestUserInput(_ context.Context, id int64, params json.RawMessage) (any, error) {
	var p ToolRequestUserInputParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("decode requested input: %w", err)
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return nil, fmt.Errorf("requested input for unknown thread %s", p.ThreadID)
	}
	inputID, err := newInputID()
	if err != nil {
		return nil, err
	}
	questions := make([]inputQuestion, 0, len(p.Questions))
	ids := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		questions = append(questions, inputQuestion{
			ID:       q.ID,
			Header:   q.Header,
			Question: q.Question,
			Options:  q.Options,
			IsOther:  q.IsOther,
		})
		ids = append(ids, q.ID)
	}
	a.mu.Lock()
	st.inputs[inputID] = &pendingInput{
		relayID:      inputID,
		nativeReqID:  id,
		nativeThread: p.ThreadID,
		turnID:       p.TurnID,
		itemID:       p.ItemID,
		questionIDs:  ids,
	}
	a.mu.Unlock()
	// The blocker is registered before the event that announces it: any
	// observer of input.requested also observes waiting_input, and the
	// runtime is unsleepable from this instant.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityWaitingInput)
	_ = a.setSessionState(m, session.StateWaitingInput)
	if err := a.publishDurable(m, api.EventInputRequested, inputRequestedPayload{
		InputID:    inputID,
		TurnID:     p.TurnID,
		ItemID:     p.ItemID,
		IsBlocking: p.IsBlocking,
		Questions:  questions,
	}); err != nil {
		return nil, err
	}
	return nil, ErrPendingResponse
}

// handleApproval fails closed. Relay configures approvalPolicy "never",
// so an approval request means the runtime is not honoring the verified
// bypass contract: the request is declined and recorded, never converted
// into human input.
func (a *Adapter) handleApproval(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var probe struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &probe)
	m, st := a.sessionByThread(probe.ThreadID)
	if st != nil {
		// Transient, not durable: declining does not fail the turn, but a
		// pinned bypass that the harness ignores must be visible live.
		_ = a.publishTransient(m, api.EventHarnessError, api.TurnEventPayload{
			TurnID: probe.TurnID,
			Error:  "native approval request received despite approvalPolicy=never; declined",
		})
	}
	// "decline" is the verified non-approving decision for command and
	// file-change approvals. Permission grants have no decline value —
	// answering with an RPC error is the fail-closed equivalent.
	return map[string]any{"decision": "decline"}, nil
}

// sessionByThread resolves the session owning a native thread.
func (a *Adapter) sessionByThread(threadID string) (*session.Managed, *sessState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sessionID, ok := a.threads[threadID]
	if !ok {
		return nil, nil
	}
	st := a.sessions[sessionID]
	if st == nil {
		return nil, nil
	}
	return st.m, st
}

func newInputID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "in_" + hex.EncodeToString(b[:]), nil
}
