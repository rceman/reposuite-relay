// Package acp implements the shared Agent Client Protocol (ACP) core used
// by the two concrete ACP harness adapters: `internal/harness/opencode`
// (`opencode acp`) and `internal/harness/devin` (`devin acp`).
//
// It exists because two vendors speak one verified standard protocol:
// JSON-RPC 2.0 over stdio with `initialize`, `session/new`, `session/load`,
// `session/prompt`, `session/cancel`, `session/update`, and
// `session/request_permission`. It is NOT a universal harness framework and
// NOT a generic JSON-RPC layer: Codex has its own protocol and does not use
// this package. Vendor-specific semantics (materialization rules, config
// options, identity shape) stay in the vendor packages.
//
// Everything here was checked against the installed binaries' own protocol
// code, not documentation: `opencode 1.17.20` (the ACP client library it
// embeds, with the exact zod schemas) and `devin 3000.10.31` (the Rust
// `agent-client-protocol` 1.0.0 crate it links). See
// docs/research/NATIVE_HARNESS_ARCHITECTURE.md.
//
// This package owns only protocol-family concerns: framing, the RPC engine,
// typed protocol messages, capability retention, the bounded diagnostic
// tail, process transport, and the permission-option policy. It never
// touches durable Relay state.
package acp

// Protocol method names, exactly as both implementations spell them.
const (
	// MethodInitialize is the handshake: client info/capabilities in,
	// agent capabilities out. It must be the first call.
	MethodInitialize = "initialize"
	// MethodSessionNew creates a native session.
	MethodSessionNew = "session/new"
	// MethodSessionLoad reattaches to an exact native session.
	MethodSessionLoad = "session/load"
	// MethodSessionPrompt submits one prompt; its RESPONSE is the terminal
	// turn result (stopReason + usage), unlike Codex's fire-and-notify
	// turn/start.
	MethodSessionPrompt = "session/prompt"
	// MethodSessionCancel is a NOTIFICATION (no response) that interrupts
	// the in-flight turn; the pending prompt call then returns
	// stopReason "cancelled".
	MethodSessionCancel = "session/cancel"
	// MethodSessionList enumerates native sessions (capability-gated).
	MethodSessionList = "session/list"
	// MethodSessionSetConfigOption mutates one advertised config option.
	MethodSessionSetConfigOption = "session/set_config_option"
	// MethodSessionSetMode switches the advertised session mode.
	MethodSessionSetMode = "session/set_mode"
	// MethodSessionUpdate is the agent→client streaming notification.
	MethodSessionUpdate = "session/update"
	// MethodRequestPermission is the agent→client approval request. It is
	// an approval mechanism, never Relay's requested-input surface.
	MethodRequestPermission = "session/request_permission"
)

// Agent→client notification/request kinds Relay understands or must answer.
const (
	// UpdateAgentMessageChunk is streamed assistant text.
	UpdateAgentMessageChunk = "agent_message_chunk"
	// UpdateAgentThoughtChunk is streamed hidden reasoning. Relay never
	// mixes it into assistant text and never persists it.
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	// UpdateUserMessageChunk echoes the user message.
	UpdateUserMessageChunk = "user_message_chunk"
	// UpdateUsage carries native context accounting ({size, used, cost?}).
	UpdateUsage = "usage_update"
	// UpdateConfigOption carries an updated config option set.
	UpdateConfigOption = "config_option_update"
	// UpdateCurrentMode carries a native mode change.
	UpdateCurrentMode = "current_mode_update"
	// UpdateSessionInfo carries native session metadata changes.
	UpdateSessionInfo = "session_info_update"
	// UpdateToolCall is a new tool invocation ({toolCallId, title, kind,
	// status, content?, locations?}).
	UpdateToolCall = "tool_call"
	// UpdateToolCallUpdate mutates a tool call ({toolCallId, status,
	// content?, locations?}).
	UpdateToolCallUpdate = "tool_call_update"
)

// Prompt stop reasons (the ACP stop-reason enum).
const (
	// StopEndTurn is a normally completed turn.
	StopEndTurn = "end_turn"
	// StopCancelled is an interrupted turn.
	StopCancelled = "cancelled"
	// StopMaxTokens, StopMaxTurnRequests, and StopRefusal are native
	// terminal conditions Relay reports as failures rather than successes.
	StopMaxTokens      = "max_tokens"
	StopMaxTurnRequest = "max_turn_requests"
	StopRefusal        = "refusal"
)

// Permission option kinds (the ACP permission-kind enum).
const (
	// KindAllowOnce approves this request only.
	KindAllowOnce = "allow_once"
	// KindAllowAlways approves this and future equivalent requests.
	KindAllowAlways = "allow_always"
	// KindRejectOnce rejects this request only.
	KindRejectOnce = "reject_once"
	// KindRejectAlways rejects this and future equivalent requests.
	KindRejectAlways = "reject_always"
)

// Permission outcome discriminators.
const (
	// OutcomeSelected means the client picked an advertised option.
	OutcomeSelected = "selected"
	// OutcomeCancelled means the client declined to decide.
	OutcomeCancelled = "cancelled"
)

// Bounds.
const (
	// MaxFrameBytes bounds one native ACP frame in either direction
	// (4 MiB). It is deliberately a distinct constant from Codex's
	// MaxFrameBytes and from the canonical Relay NDJSON event frame bound:
	// three different protocols, three different bounds. A larger or
	// malformed frame fails the connection closed — Relay never keeps
	// parsing after desynchronization.
	MaxFrameBytes = 4 << 20
	// StderrTailBytes bounds the retained native stderr diagnostics.
	StderrTailBytes = 8 << 10
	// MaxSessionIDBytes bounds a native session identity Relay will accept
	// from a harness.
	MaxSessionIDBytes = 128
)

// Version is the ACP protocol version Relay announces.
const Version = 1

// JSON-RPC error codes used by the engine.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeServerError    = -32000
)
