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
	ErrCursorTooOld      = "CURSOR_TOO_OLD"
	ErrShuttingDown      = "DAEMON_SHUTTING_DOWN"
	ErrInternal          = "INTERNAL"
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
	Key                 string `json:"key"`
	SessionID           string `json:"sessionId"`
	RuntimeID           string `json:"runtimeId"`
	RuntimeState        string `json:"runtimeState"`
	NativeSessionID     string `json:"nativeSessionId,omitempty"`
	Harness             string `json:"harness"`
	Cwd                 string `json:"cwd"`
	State               string `json:"state"`
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
type TranscriptPage struct {
	ThroughSeq uint64         `json:"throughSeq"`
	Records    []store.Record `json:"records"`
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
