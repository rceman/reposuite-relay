package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// Requested-input resolution is a two-commit-point transaction:
//
//  1. NATIVE commit point — Conn.Respond is an irreversible external
//     side effect. It is attempted at most once per input, owned
//     exclusively by the caller that claims the inputPending →
//     inputResponding transition.
//  2. DURABLE commit point — publishDurable(input.resolved). Only after
//     it succeeds is the live pending entry discarded.
//
// The sanitized durable resolution payload is built BEFORE Respond so a
// commit retry never needs the original answers again — secret plaintext
// is held only for the duration of the Respond call itself.
//
//	pending → responding → answered → committing → (deleted)
//	   ↑________| (Respond failed: retryable)
//	             ↘ answered → committing → (commit failed: answered again,
//	               retryable without a second Respond)
//
// Terminal paths (turn completion, runtime exit, session stop) never
// publish input.aborted over an inputAnswered entry — a native-accepted
// answer commits input.resolved, never the contradictory opposite.
func (a *Adapter) AnswerInput(_ context.Context, m *session.Managed, inputID string, sel []harness.InputAnswer) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	for {
		a.mu.Lock()
		pi, ok := st.inputs[inputID]
		if !ok {
			a.mu.Unlock()
			return fmt.Errorf("%w: input %s", ErrNoSuchInput, inputID)
		}
		switch pi.phase {
		case inputPending:
			pi.phase = inputResponding
			a.mu.Unlock()
			if err := a.respondNative(st, pi, sel); err != nil {
				return err
			}
			// Native commit point crossed; loop into the durable commit.
		case inputAnswered:
			pi.phase = inputCommitting
			a.mu.Unlock()
			return a.commitResolution(st, m, pi)
		default:
			// responding, committing, aborting: a side effect is in
			// flight — never send a second native response or publish a
			// second terminal record.
			a.mu.Unlock()
			return fmt.Errorf("%w: input %s resolution in flight", ErrBusy, inputID)
		}
	}
}

// respondNative performs the pending→answered half of the transaction:
// it builds the native answer AND the sanitized durable payload, then
// sends the single native response. On any failure the input reverts to
// pending (or reconciles to aborted when a terminal path visited during
// the attempt). On success the phase is inputAnswered and the sanitized
// payload is retained for the durable commit.
func (a *Adapter) respondNative(st *sessState, pi *pendingInput, sel []harness.InputAnswer) error {
	known := map[string]bool{}
	for _, q := range pi.questionIDs {
		known[q] = true
	}
	answers := map[string]ToolRequestUserInputAnswer{}
	flat := map[string][]string{}
	var redacted []string
	for _, s := range sel {
		if !known[s.QuestionID] || len(s.Answers) == 0 {
			return a.respondFailed(st, pi, fmt.Errorf("%w: question %s", ErrNoSuchInput, s.QuestionID))
		}
		answers[s.QuestionID] = ToolRequestUserInputAnswer{Answers: s.Answers}
		if pi.secret[s.QuestionID] {
			redacted = append(redacted, s.QuestionID)
			continue
		}
		flat[s.QuestionID] = s.Answers
	}
	sort.Strings(redacted)
	resolution := &inputResolvedPayload{
		InputID:  pi.relayID,
		Answers:  flat,
		Redacted: redacted,
	}

	a.mu.Lock()
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if srv == nil {
		return a.respondFailed(st, pi, fmt.Errorf("%w: input %s (runtime gone)", ErrNoSuchInput, pi.relayID))
	}
	if err := srv.Conn().Respond(pi.nativeReqID, ToolRequestUserInputResponse{Answers: answers}); err != nil {
		return a.respondFailed(st, pi, fmt.Errorf("respond to native input request: %w", err))
	}
	// Native commit point: the response was accepted on the wire. From
	// here the durable resolution must be committed — never re-Respond.
	a.mu.Lock()
	pi.phase = inputAnswered
	pi.resolution = resolution
	a.mu.Unlock()
	return nil
}

// respondFailed reverts a claimed inputResponding back to pending for a
// failure before the native commit point (no response was sent), then
// reconciles an abort that a terminal path requested meanwhile.
func (a *Adapter) respondFailed(st *sessState, pi *pendingInput, err error) error {
	a.mu.Lock()
	pi.phase = inputPending
	wantAbort := pi.wantAbort
	reason := pi.abortReason
	a.mu.Unlock()
	if wantAbort {
		a.abortPendingInput(st, pi.relayID, reason)
	}
	return err
}

// commitResolution is the durable half of the transaction. The input is
// already claimed inputCommitting. The live pending entry is deleted
// only after the durable input.resolved record commits; a failed append
// reverts the phase to inputAnswered and keeps the sanitized payload —
// a later retry commits it without touching the native side again.
func (a *Adapter) commitResolution(st *sessState, m *session.Managed, pi *pendingInput) error {
	err := a.publishDurable(m, api.EventInputResolved, pi.resolution)
	a.mu.Lock()
	if err != nil {
		pi.phase = inputAnswered
		a.mu.Unlock()
		return err
	}
	delete(st.inputs, pi.relayID)
	pi.resolution = nil
	turnInFlight := st.current != nil
	a.mu.Unlock()
	// Converge visibility: a commit landing after the turn ended settles
	// the session idle; mid-turn the turn resumes active.
	if turnInFlight {
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	} else {
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
	}
	return nil
}

// abortPendingInput durably aborts one input that never crossed the
// native commit point. The pending entry is deleted only after the
// durable input.aborted record commits; on failure it is retained and
// marked wantAbort so a later sweep or daemon-restart reconciliation can
// still land the terminal record exactly once.
func (a *Adapter) abortPendingInput(st *sessState, inputID, reason string) {
	a.mu.Lock()
	pi, ok := st.inputs[inputID]
	if !ok || pi.phase != inputPending {
		a.mu.Unlock()
		return
	}
	pi.phase = inputAborting
	a.mu.Unlock()
	err := a.publishDurable(st.m, api.EventInputAborted, inputAbortedPayload{
		InputID: inputID,
		Reason:  reason,
	})
	a.mu.Lock()
	if err != nil {
		pi.phase = inputPending
		pi.wantAbort = true
		pi.abortReason = reason
	} else {
		delete(st.inputs, inputID)
	}
	a.mu.Unlock()
}

// ReconcileInputs terminates durable requested-input records whose
// native request died with a previous runtime/daemon generation: each
// unresolved input gets input.aborted with the caller's stable reason,
// then the session leaves waiting_input. Called at daemon startup before
// the descriptor is published — a failure fails startup closed, so an
// impossible pending-input authority is never served.
func (a *Adapter) ReconcileInputs(m *session.Managed, inputIDs []string, reason string) error {
	for _, id := range inputIDs {
		if err := a.publishDurable(m, api.EventInputAborted, inputAbortedPayload{
			InputID: id,
			Reason:  reason,
		}); err != nil {
			return err
		}
	}
	return a.setSessionState(m, session.StateIdle)
}

// PendingInputs returns the unresolved Relay input IDs for a session.
func (a *Adapter) PendingInputs(sessionID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		return nil
	}
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
	secret := map[string]bool{}
	for _, q := range p.Questions {
		opts := q.Options
		if q.IsOther {
			opts = nil
		}
		questions = append(questions, inputQuestion{
			ID:       q.ID,
			Header:   q.Header,
			Question: q.Question,
			Options:  opts,
			IsOther:  q.IsOther,
			IsSecret: q.IsSecret,
		})
		ids = append(ids, q.ID)
		if q.IsSecret {
			secret[q.ID] = true
		}
	}
	// The pending entry is installed before the durable record publishes:
	// an answer racing the publish still finds it.
	a.mu.Lock()
	st.inputs[inputID] = &pendingInput{
		relayID:      inputID,
		nativeReqID:  id,
		nativeThread: p.ThreadID,
		turnID:       p.TurnID,
		itemID:       p.ItemID,
		questionIDs:  ids,
		secret:       secret,
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
