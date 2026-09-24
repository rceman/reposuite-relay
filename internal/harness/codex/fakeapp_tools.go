package codex

import (
	"fmt"
	"strings"
)

// Deterministic tool-item fixtures for telemetry-mapping tests: prompts
// prefixed "tool:<kind>" drive emitToolItems below.

// emitToolItems emits deterministic item/started + item/completed pairs for
// telemetry-mapping tests. kind selects the native shape.
func (f *fakeServer) emitToolItems(pt *pendingTurn) {
	kind := strings.TrimPrefix(pt.text, "tool:")
	f.items++
	id := fmt.Sprintf("tool_%016d", f.items)
	var item map[string]any
	switch kind {
	case "read":
		item = map[string]any{"type": "commandExecution", "id": id,
			"command": "cat src/main.go", "status": "completed",
			"exitCode": 0, "durationMs": 4,
			"commandActions": []map[string]any{
				{"type": "read", "command": "cat src/main.go", "name": "cat", "path": "src/main.go"}},
			"aggregatedOutput": "package main\n\nfunc main() {}\n"}
	case "list":
		item = map[string]any{"type": "commandExecution", "id": id,
			"command": "ls src", "status": "completed", "exitCode": 0,
			"commandActions": []map[string]any{
				{"type": "listFiles", "command": "ls src", "path": "src"}},
			"aggregatedOutput": "main.go\nutil.go\n"}
	case "search":
		item = map[string]any{"type": "commandExecution", "id": id,
			"command": "rg foo src", "status": "completed", "exitCode": 0,
			"commandActions": []map[string]any{
				{"type": "search", "command": "rg foo src", "path": "src", "query": "foo"}},
			"aggregatedOutput": "src/main.go:3:foo\n"}
	case "multi-read":
		item = map[string]any{"type": "commandExecution", "id": id,
			"command": "cat a.go b.go", "status": "completed", "exitCode": 0,
			"commandActions": []map[string]any{
				{"type": "read", "command": "cat a.go", "name": "cat", "path": "a.go"},
				{"type": "read", "command": "cat b.go", "name": "cat", "path": "b.go"}},
			"aggregatedOutput": "mixed output\n"}
	case "edit":
		item = map[string]any{"type": "fileChange", "id": id, "status": "completed",
			"changes": []map[string]any{
				{"path": "src/main.go", "kind": map[string]any{"type": "update"},
					"diff": "@@ -1,1 +1,2 @@\n+package x\n"}}}
	case "mcp":
		item = map[string]any{"type": "mcpToolCall", "id": id,
			"server": "fs", "tool": "lookup", "status": "completed",
			"result": map[string]any{"ok": true}}
	case "web":
		item = map[string]any{"type": "webSearch", "id": id,
			"query": "golang", "action": map[string]any{"type": "search", "query": "golang"}}
	case "fail":
		item = map[string]any{"type": "commandExecution", "id": id,
			"command": "false", "status": "failed", "exitCode": 1,
			"aggregatedOutput": ""}
	case "unknown":
		item = map[string]any{"type": "mysteryItem", "id": id, "data": "opaque"}
	default:
		return
	}
	f.notify("item/started", map[string]any{
		"threadId": pt.threadID, "turnId": pt.turnID, "startedAtMs": 1,
		"item": map[string]any{"type": item["type"], "id": id}})
	f.notify("item/completed", map[string]any{
		"threadId": pt.threadID, "turnId": pt.turnID, "completedAtMs": 2,
		"item": item})
}
