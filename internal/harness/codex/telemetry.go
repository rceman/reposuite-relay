package codex

import (
	"github.com/rceman/reposuite-relay/internal/telemetry"
)

// Codex → AgentEvent v1 mapping. All native knowledge lives here; the
// telemetry package stays harness-neutral.

// toolIdentity classifies a mappable item type. Returns nil for item types
// Relay does not treat as tool calls (agentMessage, plan, reasoning, user
// items, review markers, …) — unknown variants are ignored, never guessed.
func toolIdentity(it ThreadItem) (name, category string, ok bool) {
	switch it.Type {
	case "commandExecution":
		return "commandExecution", telemetry.CatShell, true
	case "fileChange":
		// A repository file mutation tool — not in the named categories.
		return "fileChange", telemetry.CatOtherRepo, true
	case "mcpToolCall":
		// MCP tool calls are not provably repository tools.
		n := it.Tool
		if it.Server != "" {
			n = it.Server + "/" + it.Tool
		}
		if n == "" {
			n = "mcpToolCall"
		}
		return n, telemetry.CatNonRepo, true
	case "dynamicToolCall":
		n := it.Tool
		if n == "" {
			n = "dynamicToolCall"
		}
		return n, telemetry.CatNonRepo, true
	case "webSearch":
		// A web search is NOT a repository search.
		return "webSearch", telemetry.CatNonRepo, true
	case "functionCallOutput":
		n := it.Name
		if n == "" {
			n = "functionCallOutput"
		}
		return n, telemetry.CatNonRepo, true
	}
	return "", "", false
}

// hasStart reports whether the item type has a meaningful start event:
// functionCallOutput only ever appears completed.
func hasStart(typ string) bool {
	return typ != "functionCallOutput"
}

// toolOK derives success from the native status/exit evidence.
func toolOK(it ThreadItem) bool {
	switch it.Type {
	case "commandExecution", "fileChange", "mcpToolCall":
		if it.Status != "completed" {
			return false
		}
		if it.Type == "commandExecution" && it.ExitCode != nil && *it.ExitCode != 0 {
			return false
		}
		return true
	case "dynamicToolCall":
		return it.Success != nil && *it.Success
	case "webSearch", "functionCallOutput":
		return true // presence in item/completed is the terminal evidence
	}
	return it.Status == "completed"
}

// toolOutput returns the bounded output metadata source bytes (the exact
// delivered payload used for bytes/digest — the payload itself is never
// emitted into telemetry).
func toolOutput(it ThreadItem) []byte {
	switch it.Type {
	case "commandExecution":
		if it.AggregatedOutput != "" {
			return []byte(it.AggregatedOutput)
		}
	case "mcpToolCall", "dynamicToolCall", "functionCallOutput":
		if len(it.Result) > 0 {
			return []byte(it.Result)
		}
	}
	return nil
}

// sourcesOf extracts SourceObserved observations for a completed item.
// Only structured source-delivery evidence qualifies:
//
//   - commandExecution with exactly ONE `read` CommandAction: Codex itself
//     classified the command as a read of that path, and aggregatedOutput
//     is the delivered content. Multiple read actions make per-file
//     attribution unprovable → no event (never a guessed split).
//   - fileChange: each change's diff IS delivered patch content.
//
// `listFiles`/`search` actions, bare paths, and prose mentions never
// produce SourceObserved — filenames are not source content.
func (a *Adapter) sourcesOf(cwd string, it ThreadItem) []telemetry.SourceObs {
	var out []telemetry.SourceObs
	switch it.Type {
	case "commandExecution":
		var reads []CommandAction
		for _, act := range it.CommandActions {
			if act.Type == "read" {
				reads = append(reads, act)
			}
		}
		if len(reads) == 1 && it.AggregatedOutput != "" {
			out = append(out, telemetry.SourceObs{
				Path:    telemetry.RelPath(reads[0].Path, cwd),
				Kind:    telemetry.ObsExplicitRead,
				CallID:  it.ID,
				Content: []byte(it.AggregatedOutput),
			})
		}
	case "fileChange":
		for _, ch := range it.Changes {
			if ch.Diff == "" {
				continue
			}
			out = append(out, telemetry.SourceObs{
				Path:    telemetry.RelPath(ch.Path, cwd),
				Kind:    telemetry.ObsDiff,
				CallID:  it.ID,
				Content: []byte(ch.Diff),
			})
		}
	}
	return out
}
