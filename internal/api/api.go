// Package api defines the local Relay HTTP/JSON wire contract (ADR-006):
// the DTOs shared by relayd and every local client (CLI, future TUI,
// future Gateway bridge). Transport mechanics live in internal/client.
//
// Stable machine error codes are part of the contract; the API version is
// negotiated through the daemon descriptor.
package api

import (
	"encoding/json"

	"github.com/rceman/reposuite-relay/internal/store"
)

// Version is the local API version, published in the daemon descriptor
// and enforced by clients.
const Version = 1

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

// DaemonInfo identifies a live daemon generation.
type DaemonInfo struct {
	InstanceID    string  `json:"instanceId"`
	PID           int     `json:"pid"`
	APIVersion    int     `json:"apiVersion"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	SessionCount  int     `json:"sessionCount"`
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
