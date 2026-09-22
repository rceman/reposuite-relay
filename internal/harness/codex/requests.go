package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

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
		// isOther offers a free-form answer path in ADDITION to any
		// structured options — it never discards them.
		questions = append(questions, inputQuestion{
			ID:       q.ID,
			Header:   q.Header,
			Question: q.Question,
			Options:  q.Options,
			IsOther:  q.IsOther,
			IsSecret: q.IsSecret,
		})
		ids = append(ids, q.ID)
		if q.IsSecret {
			secret[q.ID] = true
		}
	}
	// The entry is installed as inputAnnouncing before any durable work:
	// a terminal path racing the announcement can mark wantAbort, and
	// abortInputs must never publish input.aborted for a request whose
	// input.requested record does not yet exist.
	a.mu.Lock()
	pi := &pendingInput{
		relayID:      inputID,
		nativeReqID:  id,
		nativeThread: p.ThreadID,
		turnID:       p.TurnID,
		itemID:       p.ItemID,
		questionIDs:  ids,
		secret:       secret,
		phase:        inputAnnouncing,
		ready:        make(chan struct{}),
	}
	st.inputs[inputID] = pi
	a.mu.Unlock()
	// The blocker is registered before the event that announces it: any
	// observer of input.requested also observes waiting_input, and the
	// runtime is unsleepable from this instant.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityWaitingInput)
	// Durable waiting_input commits BEFORE the durable input.requested:
	// restart reconciliation scans sessions whose durable state signals
	// unresolved input, so no durable request may exist without it. A
	// failed state write fails the announcement closed — no requested
	// record, no answerable entry.
	if err := a.setSessionState(m, session.StateWaitingInput); err != nil {
		a.failAnnouncement(st, inputID)
		return nil, fmt.Errorf("persist waiting_input for requested input: %w", err)
	}
	if err := a.publishDurable(m, api.EventInputRequested, inputRequestedPayload{
		InputID:    inputID,
		TurnID:     p.TurnID,
		ItemID:     p.ItemID,
		IsBlocking: p.IsBlocking,
		Questions:  questions,
	}); err != nil {
		a.failAnnouncement(st, inputID)
		return nil, fmt.Errorf("publish input.requested: %w", err)
	}
	// The request is durably announced: answerable from here. If a
	// terminal path visited mid-announcement, reconcile it now —
	// input.requested exists, so the abort can commit after it.
	a.mu.Lock()
	pi.phase = inputPending
	close(pi.ready)
	wantAbort := pi.wantAbort
	abortReason := pi.abortReason
	a.mu.Unlock()
	if wantAbort {
		a.abortPendingInput(st, inputID, abortReason)
		return nil, fmt.Errorf("requested input superseded: %s", abortReason)
	}
	return nil, ErrPendingResponse
}

// failAnnouncement drops an entry whose durable announcement could not
// complete: no input.requested committed, so the input never became
// answerable and no terminal record is owed — input.aborted must never
// precede input.requested. The canonical projection then reconciles the
// session's activity/state (an announcing failure mid-turn restores
// active; a stale waiting_input can only linger durably if the rollback
// write itself fails, which restart reconciliation normalizes).
func (a *Adapter) failAnnouncement(st *sessState, inputID string) {
	a.mu.Lock()
	if pi, ok := st.inputs[inputID]; ok {
		close(pi.ready) // release answers parked on the announcement
		delete(st.inputs, inputID)
	}
	a.mu.Unlock()
	a.settleSessionState(st)
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
