package acp

import (
	"github.com/rceman/reposuite-relay/internal/telemetry"
)

// ACP tool_call/tool_call_update → AgentEvent v1 mapping. This is the
// shared protocol-family knowledge for Devin and OpenCode; each adapter
// feeds observed updates through here and the telemetry service decides
// nothing about ACP.

// toolCategory maps the ACP tool kind to the canonical ToolCategory.
// Unknown/unproven kinds fall back to the safest category — never a
// guessed repository tool.
func ToolCategory(kind string) string {
	switch kind {
	case "read":
		return telemetry.CatFileRead
	case "search":
		return telemetry.CatSearch
	case "execute":
		return telemetry.CatShell
	case "edit", "delete", "move":
		return telemetry.CatOtherRepo
	case "fetch", "think", "switch_mode", "other":
		return telemetry.CatNonRepo
	default:
		return telemetry.CatNonRepo
	}
}

// terminalStatus reports whether an ACP tool status ends the call.
func TerminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// ToolStart builds the canonical start params from a tool_call update.
// Returns nil when the update lacks a toolCallId.
func ToolStart(u Update) *telemetry.ToolStart {
	if u.ToolCallID == "" {
		return nil
	}
	name := u.Kind
	if u.Title != nil && *u.Title != "" {
		name = *u.Title
	}
	return &telemetry.ToolStart{
		CallID:   u.ToolCallID,
		ToolName: name,
		Category: ToolCategory(u.Kind),
	}
}

// ToolEnd builds the canonical completion params from a terminal
// tool_call_update (or a tool_call that arrived already terminal). kind is
// the resolved tool kind — updates do not repeat it, so the adapter passes
// the kind tracked at tool_call time.
func ToolEnd(u Update, kind string) *telemetry.ToolEnd {
	if u.ToolCallID == "" || !TerminalStatus(u.Status) {
		return nil
	}
	name := kind
	if u.Title != nil && *u.Title != "" {
		name = *u.Title
	}
	return &telemetry.ToolEnd{
		CallID:   u.ToolCallID,
		ToolName: name,
		Category: ToolCategory(kind),
		OK:       u.Status == "completed",
		Status:   u.Status,
	}
}

// ToolSources extracts SourceObserved observations from a tool update's
// structured content. Only delivered CONTENT qualifies:
//
//   - kind=read with text content → explicit_read (path from the single
//     location; multiple locations make attribution unprovable → path
//     omitted rather than guessed)
//   - diff content entries → diff observation on the delivered newText
//   - kind=search with text content → search_snippet with no path (the
//     result cannot be attributed to one file without parsing output —
//     parsing is never done)
//
// Locations without content, filename-only listings, and prose never
// produce SourceObserved.
func ToolSources(u Update, cwd, kind, path string) []telemetry.SourceObs {
	var out []telemetry.SourceObs
	if u.ToolCallID == "" {
		return nil
	}
	// path is the resolved single-location path (tracked across updates);
	// normalized repository-relative only when provably under the root.
	path = telemetry.RelPath(path, cwd)
	if path == "" && len(u.Locations) == 1 {
		path = telemetry.RelPath(u.Locations[0].Path, cwd)
	}
	for _, c := range u.ToolContents() {
		switch {
		case c.Type == "diff" && c.NewText != nil && *c.NewText != "":
			out = append(out, telemetry.SourceObs{
				Path:    telemetry.RelPath(firstNonEmpty(c.Path, path), cwd),
				Kind:    telemetry.ObsDiff,
				CallID:  u.ToolCallID,
				Content: []byte(*c.NewText),
			})
		case c.Type == "content" && c.Content != nil && c.Content.Text != "":
			switch kind {
			case "read":
				out = append(out, telemetry.SourceObs{
					Path:    path,
					Kind:    telemetry.ObsExplicitRead,
					CallID:  u.ToolCallID,
					Content: []byte(c.Content.Text),
				})
			case "search":
				out = append(out, telemetry.SourceObs{
					Kind:    telemetry.ObsSearchSnippet,
					CallID:  u.ToolCallID,
					Content: []byte(c.Content.Text),
				})
			}
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// FirstToken prefers the cached-read measurement over cached-write for the
// canonical cached-input field; absent values stay absent.
func FirstToken(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}

// toolMeta is the remembered tool-call identity — updates do not repeat
// kind/locations.
type ToolMeta struct {
	Kind string
	Path string
}
