package codex

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// --- notifications --------------------------------------------------
func (a *Adapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case NotifyTurnStarted:
		a.onTurnStarted(params)
	case NotifyAgentMessage:
		a.onAgentMessageDelta(params)
	case NotifyItemCompleted:
		a.onItemCompleted(params)
	case NotifyTurnCompleted:
		a.onTurnCompleted(params)
	case NotifyTokenUsage:
		a.onTokenUsage(params)
	case NotifyRateLimits:
		a.onRateLimits(params)
	case NotifyError:
		a.onError(params)
	case NotifyThreadStarted, NotifyServerReqResolv:
		// Informational: Relay's own events already record thread start
		// (harness.started) and input resolution (input.resolved).
	default:
		// Unknown notifications are ignored deliberately: the app-server
		// adds notifications over time and Relay must not fail on them.
	}
}

func (a *Adapter) onTurnStarted(params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     Turn   `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current == nil {
		st.current = &activeTurn{
			nativeThreadID: p.ThreadID,
			turnID:         p.Turn.ID,
		}
	} else if st.current.turnID == "" {
		st.current.turnID = p.Turn.ID
	}
	a.mu.Unlock()
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)
	_ = a.setSessionState(m, session.StateActive)
}

func (a *Adapter) onAgentMessageDelta(params json.RawMessage) {
	var p AgentMessageDeltaNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current != nil {
		st.current.agentText += p.Delta
		if p.ItemID != "" {
			st.current.agentItemID = p.ItemID
		}
	}
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMessageAgentDelta, api.MessageCompletedPayload{
		TurnID: p.TurnID,
		ItemID: p.ItemID,
		Text:   p.Delta,
	})
}

func (a *Adapter) onItemCompleted(params json.RawMessage) {
	var p ItemCompletedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.Item.Type != "agentMessage" {
		return
	}
	_, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current != nil {
		st.current.agentText = p.Item.Text
		st.current.agentItemID = p.Item.ID
	}
	a.mu.Unlock()
}

func (a *Adapter) onTurnCompleted(params json.RawMessage) {
	var p TurnCompletedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	// The turn stays in flight until its terminal durable record is
	// published: a concurrent prompt must never interleave its own
	// acceptance events with the completion of the previous turn.
	a.mu.Lock()
	cur := st.current
	text, itemID := "", ""
	if cur != nil {
		text, itemID = cur.agentText, cur.agentItemID
	}
	firstTurn := cur != nil && cur.firstTurn
	nativeID := st.nativeID
	materialized := st.materialized
	a.mu.Unlock()

	// A completed first turn proves the native session is materialized:
	// only now is the exact native identity persisted. Materialization is
	// orthogonal to generation — the binding already bumped it.
	if p.Turn.Status == "completed" && firstTurn && nativeID != "" && !materialized {
		idle := session.StateIdle
		if err := a.deps.Materialize(m, harness.SessionUpdate{
			NativeSessionID: &nativeID,
			State:           &idle,
		}); err != nil {
			_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
				TurnID: p.Turn.ID,
				Error:  "persist native session: " + err.Error(),
			})
		} else {
			a.mu.Lock()
			st.materialized = true
			a.mu.Unlock()
			_ = a.publishDurable(m, api.EventNativeSession, api.NativeSessionPayload{
				NativeSessionID: nativeID,
				Generation:      m.Snapshot().Generation,
			})
		}
	}

	switch p.Turn.Status {
	case "completed":
		// Prefer the completed turn's own agent message over accumulated
		// deltas — deltas are a live view, the item is the final text.
		for _, it := range p.Turn.Items {
			if it.Type == "agentMessage" && it.Text != "" {
				text, itemID = it.Text, it.ID
			}
		}
		_ = a.publishDurable(m, api.EventMessageAgentCompleted, api.MessageCompletedPayload{
			TurnID: p.Turn.ID,
			ItemID: itemID,
			Text:   text,
		})
	case "interrupted":
		_ = a.publishDurable(m, api.EventTurnInterrupted, api.TurnEventPayload{TurnID: p.Turn.ID})
	case "failed":
		msg := "turn failed"
		if p.Turn.Error != nil && p.Turn.Error.Message != "" {
			msg = p.Turn.Error.Message
		}
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: p.Turn.ID,
			Error:  msg,
		})
	default:
		// Unknown terminal status: record it rather than inventing a
		// success or failure.
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{
			TurnID: p.Turn.ID,
			Error:  "turn ended with status " + p.Turn.Status,
		})
	}

	// Terminal record published: the turn is no longer in flight.
	a.mu.Lock()
	st.current = nil
	a.mu.Unlock()
	// Unresolved input cannot outlive its turn.
	a.abortInputs(st, "turn "+p.Turn.Status)
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
	_ = a.setSessionState(m, session.StateIdle)
}

func (a *Adapter) onTokenUsage(params json.RawMessage) {
	var p ThreadTokenUsageUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.metrics == nil {
		st.metrics = &Metrics{}
	}
	st.metrics.ModelContextWindow = p.TokenUsage.ModelContextWindow
	last, total := p.TokenUsage.Last, p.TokenUsage.Total
	st.metrics.Last, st.metrics.Total = &last, &total
	metrics := *st.metrics
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMetricsUpdated, metricsPayload{
		Kind:           "tokenUsage",
		Metrics:        metrics,
		SessionMetrics: derefMetrics(canonicalMetrics(&metrics)),
	})
}

func (a *Adapter) onRateLimits(params json.RawMessage) {
	var p AccountRateLimitsUpdatedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	a.mu.Lock()
	var target *sessState
	for _, st := range a.sessions {
		if st.liveKey == RuntimeKey {
			target = st
			break
		}
	}
	if target == nil {
		a.mu.Unlock()
		return
	}
	if target.metrics == nil {
		target.metrics = &Metrics{}
	}
	rl := p.RateLimits
	target.metrics.RateLimits = &rl
	metrics := *target.metrics
	m := target.m
	a.mu.Unlock()
	_ = a.publishTransient(m, api.EventMetricsUpdated, metricsPayload{
		Kind:           "rateLimits",
		Metrics:        metrics,
		SessionMetrics: derefMetrics(canonicalMetrics(&metrics)),
	})
}

func (a *Adapter) onError(params json.RawMessage) {
	var p struct {
		Message  string `json:"message"`
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.ThreadID == "" {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	_ = a.publishTransient(m, api.EventHarnessError, api.TurnEventPayload{
		TurnID: p.TurnID,
		Error:  p.Message,
	})
}
