// Package api defines the local Relay HTTP/JSON wire contract (ADR-006):
// the DTOs shared by relayd and every client: the CLI, the embedded
// Web Admin, and external machine integrations (Gateway/GPT Tunnel).
// Transport mechanics live in internal/client.
//
// Stable machine error codes are part of the contract; the API version is
// negotiated through the daemon descriptor.
package api

import (
	"encoding/json"

	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/store"
)

// Version is the local API version, published in the daemon descriptor
// and enforced by clients. Version 2 hard-cut the prompt response to the
// harness-neutral nativeSessionId field (it replaced the Codex-specific
// nativeThreadId); no legacy compatibility is carried.
const Version = 2

// DescriptorVersion is the descriptor schema version.
const DescriptorVersion = 1

// Descriptor is the runtime discovery record published at
// <relay>/run/daemon.json (0600) by the daemon generation that owns the
// singleton lock. Clients validate it strictly and fail closed on
// anything that is not exactly a loopback relayd.
type Descriptor struct {
	Version     int    `json:"version"`
	InstanceID  string `json:"instanceId"`
	PID         int    `json:"pid"`
	Endpoint    string `json:"endpoint"` // http://127.0.0.1:<port>
	BearerToken string `json:"bearerToken"`
}

// Stable machine-oriented error codes.
const (
	ErrInvalidRequest    = "INVALID_REQUEST"
	ErrInvalidSessionKey = "INVALID_SESSION_KEY"
	ErrUnauthorized      = "UNAUTHORIZED"
	ErrSessionExists     = "SESSION_EXISTS"
	ErrSessionNotFound   = "SESSION_NOT_FOUND"
	// ErrCursorTooOld means the requested after=N cursor is below the
	// session's replay floor: exact replay cannot be guaranteed (the ring
	// overwrote it, or it predates this daemon generation).
	ErrCursorTooOld = "CURSOR_TOO_OLD"
	// ErrCursorAhead means the requested after=N cursor is above the
	// session's current canonical event cursor — a cursor from the future
	// is never established as a subscription.
	ErrCursorAhead = "CURSOR_AHEAD"
	// ErrHistoryRecordTooLarge means a canonical durable record itself
	// exceeds the bounded history-page byte budget; the transcript is
	// intact but cannot be served within the response bound.
	ErrHistoryRecordTooLarge = "HISTORY_RECORD_TOO_LARGE"
	ErrShuttingDown          = "DAEMON_SHUTTING_DOWN"
	ErrInternal              = "INTERNAL"

	// ErrSessionBusy means the session already has an in-flight turn.
	ErrSessionBusy = "SESSION_BUSY"
	// ErrNoActiveTurn means cancel was requested with no in-flight turn.
	ErrNoActiveTurn = "NO_ACTIVE_TURN"
	// ErrNativeSessionLost means the exact recorded native session could
	// not be resumed. Relay never substitutes a different native session.
	ErrNativeSessionLost = "NATIVE_SESSION_LOST"
	// ErrUnknownInput means the requested-input ID is unknown or already
	// resolved for this session.
	ErrUnknownInput = "UNKNOWN_INPUT"
	// ErrRuntimeUnavailable means the harness runtime could not be started
	// or initialized.
	ErrRuntimeUnavailable = "RUNTIME_UNAVAILABLE"
	// ErrInvalidConfig means the requested model/mode is not acceptable.
	ErrInvalidConfig = "INVALID_CONFIG"
	// ErrUnsupportedOperation means the operation is not supported by this
	// harness — a capability mismatch, never an internal failure.
	ErrUnsupportedOperation = "UNSUPPORTED_OPERATION"
	// ErrProjectNotFound names an unknown Relay-local project ID.
	ErrProjectNotFound = "PROJECT_NOT_FOUND"
	// ErrProjectRootConflict means the canonical project root is already
	// owned by another project.
	ErrProjectRootConflict = "PROJECT_ROOT_CONFLICT"
)

// Canonical size bounds. These are part of the wire contract and are used
// identically by event publication, the NDJSON event server, and the
// canonical client scanner — an event that cannot be decoded by the
// canonical client must never be published.
const (
	// MaxEventFrameBytes bounds one canonical NDJSON event frame
	// (4 MiB). Oversized publication fails explicitly before any
	// subscriber can receive an undecodable frame.
	MaxEventFrameBytes = 4 << 20
	// HistoryPageMaxBytes bounds one transcript page response (8 MiB,
	// deliberately above MaxEventFrameBytes so any validly published
	// record always fits a page).
	HistoryPageMaxBytes = 8 << 20
)

// ErrorBody is the single error envelope for every failed request.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a stable code plus a human message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DaemonInfo identifies a live daemon generation. Session counts are a
// cheap in-memory projection: ActiveSessions are bound to a live runtime
// generation, ColdSessions are durable but unbound (they wake on next
// use). Neither count wakes a session or touches a native runtime.
type DaemonInfo struct {
	InstanceID     string  `json:"instanceId"`
	PID            int     `json:"pid"`
	APIVersion     int     `json:"apiVersion"`
	UptimeSeconds  float64 `json:"uptimeSeconds"`
	SessionCount   int     `json:"sessionCount"`
	ActiveSessions int     `json:"activeSessions"`
	ColdSessions   int     `json:"coldSessions"`
}

// SessionInfo is the wire view of a durable RelaySession plus its optional
// live HarnessRuntime. For a COLD session runtimeId is "", pid 0,
// generationStartedAt "", runtimeState "cold". Credentials are never here.
type SessionInfo struct {
	Key             string `json:"key"`
	SessionID       string `json:"sessionId"`
	RuntimeID       string `json:"runtimeId"`
	RuntimeState    string `json:"runtimeState"`
	NativeSessionID string `json:"nativeSessionId,omitempty"`
	Harness         string `json:"harness"`
	Cwd             string `json:"cwd"`
	State           string `json:"state"`
	// Activity is the per-session turn state (idle/active/waiting_input) —
	// separate from the possibly shared runtime process state.
	Activity            string `json:"activity"`
	Model               string `json:"model,omitempty"`
	Mode                string `json:"mode,omitempty"`
	Generation          int    `json:"generation"`
	PID                 int    `json:"pid"`
	CreatedAt           string `json:"createdAt"`
	GenerationStartedAt string `json:"generationStartedAt"`
	// Metrics is the last-known in-memory harness accounting, when the
	// harness reported any. It is never persisted and never wakes a
	// runtime to produce.
	Metrics *harness.SessionMetrics `json:"metrics,omitempty"`
	// ProjectID/ProjectName are DERIVED presentation fields computed at
	// read time from the local project catalog and the session cwd —
	// never persisted on the RelaySession. Either both are present or
	// neither (an unmatched session is Ungrouped).
	ProjectID   string `json:"projectId,omitempty"`
	ProjectName string `json:"projectName,omitempty"`
}

// Canonical durable event types. These are persisted in the session
// transcript and replayed to clients; the set is closed and documented.
const (
	// EventMessageUser records an accepted user prompt.
	EventMessageUser = "message.user"
	// EventMessageAgentCompleted records a completed agent message.
	EventMessageAgentCompleted = "message.agent.completed"
	// EventTurnInterrupted records an interrupted turn.
	EventTurnInterrupted = "turn.interrupted"
	// EventTurnFailed records a failed turn or prompt submission.
	EventTurnFailed = "turn.failed"
	// EventRuntimeExited records the death of a harness runtime generation.
	EventRuntimeExited = "runtime.exited"
	// EventHarnessStarted records that a runtime generation took ownership
	// of a session (native thread started or resumed).
	EventHarnessStarted = "harness.started"
	// EventInputRequested records a native requested-input request.
	EventInputRequested = "input.requested"
	// EventInputResolved records a user answer to requested input.
	EventInputResolved = "input.resolved"
	// EventInputAborted records requested input that can no longer be
	// answered (runtime death, session stop).
	EventInputAborted = "input.aborted"
	// EventNativeSession records proven native materialization: from this
	// point the exact native session ID is durable.
	EventNativeSession = "session.native"
	// EventConfigChanged records an accepted model/mode change.
	EventConfigChanged = "session.config"
)

// Canonical transient event types (live stream only, never persisted).
const (
	// EventMessageAgentDelta is a streamed agent message chunk.
	EventMessageAgentDelta = "message.agent.delta"
	// EventMetricsUpdated carries token usage / rate limit updates.
	EventMetricsUpdated = "metrics.updated"
	// EventHarnessError carries a harness-reported error notification.
	EventHarnessError = "harness.error"
)

// CreateRequest is the body of every built-in creation route
// (POST /v1/sessions/{codex|devin|opencode}). There are no executable/args
// fields: the daemon runs only its own built-in native adapters. Creating a
// session is a durable, runtime-free operation (COLD) — no harness process
// is started until the first prompt.
type CreateRequest struct {
	Key   string `json:"key"`
	Cwd   string `json:"cwd"`
	Model string `json:"model,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// PromptRequest is the body of POST /v1/sessions/{key}/prompt.
type PromptRequest struct {
	Text   string `json:"text"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// PromptResponse reports the accepted native turn. NativeSessionID is the
// exact native session identity in force for that turn, whatever the
// harness calls it (a Codex thread, an OpenCode `ses_*`, a Devin slug).
type PromptResponse struct {
	Daemon          DaemonInfo `json:"daemon"`
	Key             string     `json:"key"`
	TurnID          string     `json:"turnId"`
	NativeSessionID string     `json:"nativeSessionId"`
	RuntimeID       string     `json:"runtimeId"`
}

// CancelResponse reports an accepted interruption.
type CancelResponse struct {
	Daemon DaemonInfo `json:"daemon"`
	Key    string     `json:"key"`
	TurnID string     `json:"turnId"`
}

// InputAnswer is one answer to one requested-input question.
type InputAnswer struct {
	QuestionID string   `json:"questionId"`
	Answers    []string `json:"answers"`
}

// InputRequest is the body of POST /v1/sessions/{key}/input.
type InputRequest struct {
	InputID string        `json:"inputId"`
	Answers []InputAnswer `json:"answers"`
}

// InputResponse reports an accepted answer.
type InputResponse struct {
	Daemon  DaemonInfo `json:"daemon"`
	Key     string     `json:"key"`
	InputID string     `json:"inputId"`
}

// ConfigRequest is the body of PATCH /v1/sessions/{key}/config. Both
// fields are optional; an absent field is left unchanged. The accepted
// values take effect at the next native thread start/resume or turn.
type ConfigRequest struct {
	Model *string `json:"model,omitempty"`
	Mode  *string `json:"mode,omitempty"`
}

// FixtureRequest is the body of POST /v1/sessions/fixture. There are no
// executable/args fields: the daemon never executes client commands.
type FixtureRequest struct {
	Key string `json:"key"`
	Cwd string `json:"cwd"`
}

// DaemonResponse is the response shape for operations that report daemon
// state (daemon info, shutdown, session delete).
type DaemonResponse struct {
	Daemon DaemonInfo `json:"daemon"`
}

// SessionResponse is the response shape for single-session operations.
type SessionResponse struct {
	Daemon  DaemonInfo  `json:"daemon"`
	Session SessionInfo `json:"session"`
}

// SessionList is the response of GET /v1/sessions.
type SessionList struct {
	Daemon   DaemonInfo    `json:"daemon"`
	Sessions []SessionInfo `json:"sessions"`
}

// TranscriptPage is the response of GET /v1/sessions/{key}/transcript.
// Records are durable transcript records, not rendered rows.
//
// ThroughSeq is the canonical event cursor at snapshot time — the exact
// seq a client subscribes after (events?after=throughSeq) to continue
// with no gap. It is NOT the last durable record seq: transient events
// and reserved sequence blocks advance the cursor beyond durable
// records, and those seq values can never be replayed.
type TranscriptPage struct {
	ThroughSeq uint64         `json:"throughSeq"`
	Records    []store.Record `json:"records"`
	// HasMoreBefore reports that older durable records exist beyond the
	// returned window (count or byte bound reached).
	HasMoreBefore bool `json:"hasMoreBefore"`
}

// Limits for the transcript endpoint.
const (
	TranscriptDefaultLimit = 200
	TranscriptMaxLimit     = 1000
)

// Marshal marshals any API value (helper for tests).
func Marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
