package acp

// Deterministic tool-call fixture emission (FakeTools mode) — drives the
// adapter tool_call/tool_call_update → AgentEvent mapping tests.

import "encoding/json"

// emitToolCalls emits a deterministic ACP tool-call lifecycle: a read tool
// delivering source content (source_observed evidence), a listing tool
// (filename-only — never an observation), and a failed execute.
func (a *fakeAgent) emitToolCalls(sessionID string) {
	a.emit(sessionID, Update{
		SessionUpdate: UpdateToolCall,
		ToolCallID:    "tc-read",
		Title:         strp("read src/main.go"),
		Kind:          "read",
		Status:        "in_progress",
		Locations:     []ToolLocation{{Path: "src/main.go"}},
	})
	a.emit(sessionID, Update{
		SessionUpdate: UpdateToolCallUpdate,
		ToolCallID:    "tc-read",
		Status:        "completed",
		Content:       toolContentText("package main\n\nfunc main() {}\n"),
	})
	a.emit(sessionID, Update{
		SessionUpdate: UpdateToolCall,
		ToolCallID:    "tc-list",
		Title:         strp("list src"),
		Kind:          "other",
		Status:        "in_progress",
		Locations:     []ToolLocation{{Path: "src"}},
	})
	a.emit(sessionID, Update{
		SessionUpdate: UpdateToolCallUpdate,
		ToolCallID:    "tc-list",
		Status:        "completed",
	})
	a.emit(sessionID, Update{
		SessionUpdate: UpdateToolCallUpdate,
		ToolCallID:    "tc-exec",
		Title:         strp("run tests"),
		Kind:          "execute",
		Status:        "failed",
	})
}

// toolContentText encodes one ACP tool content item carrying text.
func toolContentText(text string) json.RawMessage {
	b, _ := json.Marshal([]ToolContent{{
		Type: "content",
		Content: &ContentBlock{
			Type: "text",
			Text: text,
		},
	}})
	return b
}

func strp(s string) *string { return &s }
