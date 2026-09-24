// Package telemetry implements Relay's observer-only RepoDex AgentEvent v1
// pipeline: native harness observation → canonical event → bounded queue →
// batched delivery to RepoDex → durable ACK.
//
// The package is harness-neutral: it knows the canonical AgentEvent v1
// contract (reposuite.agent-event.v1, defined by RepoDex c2bd73a) and nothing
// about Codex/Devin/OpenCode wire protocols. Harness adapters translate their
// native observations into the typed Params here; vendor DTO knowledge stays
// inside internal/harness/*.
//
// Authority boundary: Relay → RepoDex is TELEMETRY ONLY. There is no query
// path, no RepoDex lifecycle control, and no RepositoryView resolution here.
package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// SchemaID is the canonical AgentEvent envelope discriminator (RepoDex
// reposuite.agent-event.v1). Any other value fails RepoDex validation.
const SchemaID = "reposuite.agent-event.v1"

// Canonical event families Relay emits. Relay only ever produces types it
// explicitly supports; unknown future types are RepoDex's forward-compat
// concern.
const (
	TypeSessionStarted   = "session_started"
	TypeSessionCompleted = "session_completed"
	TypeModelCallDone    = "model_call_completed"
	TypeToolCallStarted  = "tool_call_started"
	TypeToolCallDone     = "tool_call_completed"
	TypeSourceObserved   = "source_observed"
	TypeFinalAnswer      = "final_answer"
)

// RepoDex ToolCategory values (model.rs ToolCategory, snake_case).
const (
	CatSearch     = "search"
	CatFileRead   = "file_read"
	CatDirList    = "directory_list"
	CatGitInspect = "git_inspection"
	CatShell      = "shell"
	CatOtherRepo  = "other_repository_tool"
	CatNonRepo    = "non_repository_tool"
)

// RepoDex ObservationKind values (snake_case).
const (
	ObsExplicitRead  = "explicit_read"
	ObsSearchSnippet = "search_snippet"
	ObsSymbolPreview = "symbol_preview"
	ObsDiff          = "diff"
	ObsOther         = "other"
)

// SeqSessionStarted is the reserved telemetry sequence for the session's
// session_started event. Its payload uses only immutable facts (session cwd,
// durable creation time, harness identity) so re-emission after a daemon
// restart is byte-identical and RepoDex dedups it safely.
const SeqSessionStarted uint64 = 0

// Event is the canonical AgentEvent v1 envelope — the exact RepoDex wire
// shape. field names are fixed by the contract.
type Event struct {
	Schema       string          `json:"schema"`
	EventID      string          `json:"event_id"`
	SessionID    string          `json:"session_id"`
	Sequence     uint64          `json:"sequence"`
	Timestamp    string          `json:"timestamp"`
	Source       Source          `json:"source"`
	Type         string          `json:"type"`
	Data         json.RawMessage `json:"data"`
	ProjectID    string          `json:"project_id,omitempty"`
	RepositoryID string          `json:"repository_id,omitempty"`
	RepoHead     string          `json:"repo_head,omitempty"`
	// InvestigationID is Task authority — Relay never sets it (the Gateway
	// owns Task authority; Relay telemetry does not claim one).
	InvestigationID string `json:"investigation_id,omitempty"`
	// ContentDigest is left empty: RepoDex computes the semantic digest
	// server-side for dedup/conflict detection.
	ContentDigest string `json:"content_digest,omitempty"`
}

// Source is the producer provenance object (required fields per schema).
type Source struct {
	Runtime         string `json:"runtime"`
	Adapter         string `json:"adapter"`
	AdapterVersion  string `json:"adapter_version"`
	NativeSessionID string `json:"native_session_id,omitempty"`
}

// Validate mirrors RepoDex ingest.rs validate_event exactly so malformed
// canonical events never leave Relay knowingly.
func (e Event) Validate() error {
	if e.Schema != SchemaID {
		return fmt.Errorf("schema %q", e.Schema)
	}
	if e.EventID == "" {
		return fmt.Errorf("event_id empty")
	}
	if e.SessionID == "" {
		return fmt.Errorf("session_id empty")
	}
	if e.EventID != fmt.Sprintf("%s:%d", e.SessionID, e.Sequence) {
		return fmt.Errorf("event_id %q != session:sequence", e.EventID)
	}
	if e.Type == "" {
		return fmt.Errorf("type empty")
	}
	ts := e.Timestamp
	if !strings.Contains(ts, "T") || !(strings.HasSuffix(ts, "Z") || strings.Contains(ts, "+")) {
		return fmt.Errorf("timestamp not RFC3339 UTC")
	}
	if e.Sequence == math.MaxUint64 {
		return fmt.Errorf("sequence invalid")
	}
	if len(e.Data) == 0 {
		return fmt.Errorf("data empty")
	}
	return nil
}

// Encode marshals the canonical event once — the resulting bytes are the
// identity Relay enqueues, spools, and retries with. Retries never
// reconstruct an event from mutable state.
func (e Event) Encode() ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	return b, nil
}

// --- canonical data payloads -------------------------------------------------

// SessionStartedData — only immutable facts so re-emission is byte-stable.
type SessionStartedData struct {
	Repository string `json:"repository,omitempty"` // session working root (durable cwd)
}

// SessionCompletedData — a real terminal RelaySession lifecycle point.
type SessionCompletedData struct {
	Reason string `json:"reason,omitempty"`
}

// ModelCallCompletedData carries actual provider/harness usage. One Relay
// turn is not necessarily one model call, so turn-scoped usage is emitted
// with UsageEstimated=true (canonical proxy boundary); token values are
// always copied exactly from authoritative native accounting, never
// estimated.
type ModelCallCompletedData struct {
	ModelCallID        string `json:"model_call_id"`
	Model              string `json:"model,omitempty"`
	InputTokens        *int64 `json:"input_tokens,omitempty"`
	OutputTokens       *int64 `json:"output_tokens,omitempty"`
	CachedInputTokens  *int64 `json:"cached_input_tokens,omitempty"`
	CachedOutputTokens *int64 `json:"cached_output_tokens,omitempty"`
	ReasoningTokens    *int64 `json:"reasoning_tokens,omitempty"`
	DurationMs         *int64 `json:"duration_ms,omitempty"`
	Status             string `json:"status,omitempty"`
	UsageEstimated     bool   `json:"usage_estimated"`
}

// ToolCallStartedData for tool_call_started.
type ToolCallStartedData struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Category   string `json:"category"`
}

// ToolCallCompletedData for tool_call_completed. Output is metadata only —
// bytes/digest, never the payload itself.
type ToolCallCompletedData struct {
	ToolCallID   string `json:"tool_call_id"`
	OK           bool   `json:"ok"`
	DurationMs   *int64 `json:"duration_ms,omitempty"`
	OutputBytes  *int64 `json:"output_bytes,omitempty"`
	OutputDigest string `json:"output_digest,omitempty"`
	Status       string `json:"status,omitempty"`
}

// SourceObservedData for source_observed. Emitted only when actual source
// CONTENT was delivered to the agent by a tool result — never for a bare
// path mention.
type SourceObservedData struct {
	Path            string `json:"path,omitempty"`
	ObservationKind string `json:"observation_kind"`
	ToolCallID      string `json:"tool_call_id,omitempty"`
	Bytes           *int64 `json:"bytes,omitempty"`
	LineStart       *int64 `json:"line_start,omitempty"`
	LineEnd         *int64 `json:"line_end,omitempty"`
	ContentDigest   string `json:"content_digest,omitempty"`
	FileDigest      string `json:"file_content_digest,omitempty"`
	ByteStart       *int64 `json:"byte_start,omitempty"`
	ByteEnd         *int64 `json:"byte_end,omitempty"`
}

// FinalAnswerData for final_answer — the runtime-visible completed agent
// output, never a Relay-generated summary.
type FinalAnswerData struct {
	Content       string `json:"content,omitempty"`
	ContentBytes  *int64 `json:"content_bytes,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
}
