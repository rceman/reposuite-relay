# AgentEvent v1 — harness mapping

## Envelope

```text
schema      = "reposuite.agent-event.v1" (const)
event_id    = "{relay_session_id}:{telemetry_seq}"
session_id  = RelaySession.ID (stable logical identity)
sequence    = per-session durable telemetry sequence
              (TelemetrySeqWatermark, independent of the canonical
              transcript sequence; reserved in blocks of 256)
timestamp   = UTC RFC3339Nano at emission
source      = {runtime: codex|devin|opencode,
               adapter: "reposuite-relay",
               adapter_version: relay version,
               native_session_id?: exact native id when materialized}
project_id  = presentation-catalog project id when the session cwd matches
repository_id / repo_head / investigation_id = unset (no authority)
```

`session_started` is emitted lazily at the first observation of a session,
always at sequence 0. After a daemon restart it is re-emitted identically
(event_id dedup on the RepoDex side); dynamic events resume strictly after
the durable watermark — sequence values are never reused.

## Codex (`internal/harness/codex`)

| Native observation                                   | Canonical event |
|------------------------------------------------------|-----------------|
| `item/started` for mappable tool item                | tool_call_started (tool_call_id = native item id) |
| `item/completed` commandExecution/fileChange/mcpToolCall/dynamicToolCall | tool_call_completed (ok from status/exitCode; output_bytes/digest only) |
| commandExecution w/ exactly one `read` action + non-empty aggregatedOutput | source_observed {path, explicit_read, bytes, content_digest} |
| fileChange diffs (non-empty)                          | source_observed {path, diff} |
| `item/completed` agentMessage                        | agent_message (verbatim text) |
| `turn/completed` status=completed                    | final_answer (verbatim text) |
| `thread/tokenUsage/updated` (`last` breakdown)       | model_call_completed {model_call_id:"turn:<id>", exact counters, usage_estimated=true} |
| session delete                                       | session_completed {reason:"session_deleted"} |

Never observed as source: `listFiles`, `search` actions, multi-file `cat`
aggregates (attribution unprovable), bare paths, prose.

Tools still marked started at turn end are completed with the turn's
terminal status (sweep). Completed items only visible inside the terminal
turn object are emitted by a fallback pass (item-not-seen dedup by id).

## ACP — Devin and OpenCode (`internal/harness/acp` + adapters)

| ACP update                              | Canonical event |
|-----------------------------------------|-----------------|
| `tool_call` (toolCallId, kind, title, locations) | tool_call_started |
| `tool_call`/`tool_call_update` terminal status | tool_call_completed (ok = status=="completed") |
| kind=read + text content delivered      | source_observed {explicit_read, single-location path} |
| diff content entries                    | source_observed {diff, path, newText digest} |
| kind=search + text content              | source_observed {search_snippet, no path} |
| prompt result stopReason=end_turn       | final_answer (verbatim text) |
| prompt result Usage                     | model_call_completed {turn-scoped, exact fields, usage_estimated} |

ACP `kind` → category: read→file_read, search→search, execute→shell,
edit/delete/move→other_repository_tool, fetch/think/other→non_repository_tool.

Updates do not repeat `kind`/`locations`; each adapter tracks them per
toolCallId in memory until the terminal update.

`agent_thought_chunk` content is never carried — hidden reasoning is
excluded by design; `thought_tokens` map to `reasoning_tokens`.

## Source-observation policy (shared)

`source_observed` means source CONTENT was delivered to the agent inside a
tool result. Filename-only references, directory listings, search result
paths, generic shell output, and prose claims never emit it. Content is
never re-sent: events carry `bytes` + `sha256` digest only. Paths are
repo-relative when provably under the session root, else omitted.
