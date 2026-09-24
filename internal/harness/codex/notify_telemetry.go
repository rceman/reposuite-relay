package codex

// Codex item → RepoDex telemetry wiring. The mapper lives in
// telemetry.go; these handlers own notification dispatch, per-turn
// tracking, and the turn-end sweep/fallback.

import (
	"encoding/json"

	"github.com/rceman/reposuite-relay/internal/telemetry"
)

// onItemStarted emits telemetry tool_call_started for mappable native tool
// items — the exact native item id becomes the tool_call_id.
func (a *Adapter) onItemStarted(params json.RawMessage) {
	var p ItemStartedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	name, cat, ok := toolIdentity(p.Item)
	if !ok || !hasStart(p.Item.Type) {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	a.mu.Lock()
	if st.current != nil {
		if st.current.tools == nil {
			st.current.tools = map[string]*toolObs{}
		}
		if p.Item.ID != "" {
			st.current.tools[p.Item.ID] = &toolObs{
				name:     name,
				category: cat,
			}
		}
	}
	a.mu.Unlock()
	a.deps.Telemetry.ToolCallStarted(m, telemetry.ToolStart{
		CallID: p.Item.ID, ToolName: name, Category: cat,
	})
}

func (a *Adapter) onItemCompleted(params json.RawMessage) {
	var p ItemCompletedNotification
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	m, st := a.sessionByThread(p.ThreadID)
	if st == nil {
		return
	}
	if p.Item.Type == "agentMessage" {
		a.mu.Lock()
		if st.current != nil {
			st.current.agentText = p.Item.Text
			st.current.agentItemID = p.Item.ID
		}
		a.mu.Unlock()
		if p.Item.Text != "" {
			a.deps.Telemetry.AgentMessage(m, p.Item.Text)
		}
		return
	}
	// Tool/source telemetry from the completed native item.
	if name, cat, ok := toolIdentity(p.Item); ok {
		a.mu.Lock()
		if st.current != nil {
			if st.current.tools == nil {
				st.current.tools = map[string]*toolObs{}
			}
			if t := st.current.tools[p.Item.ID]; t != nil {
				t.completed = true
			} else if p.Item.ID != "" {
				st.current.tools[p.Item.ID] = &toolObs{
					name:      name,
					category:  cat,
					completed: true,
				}
			}
		}
		a.mu.Unlock()
		out := toolOutput(p.Item)
		a.deps.Telemetry.ToolCallCompleted(m, telemetry.ToolEnd{
			CallID: p.Item.ID, ToolName: name, Category: cat,
			OK: toolOK(p.Item), Status: p.Item.Status,
			DurationMs: p.Item.DurationMs, Output: out,
		})
		for _, so := range a.sourcesOf(m.Snapshot().Cwd, p.Item) {
			a.deps.Telemetry.SourceObserved(m, so)
		}
	}
}
