// Package harness defines the daemon-facing contract every native harness
// adapter implements (ADR-005): Relay operations expressed in Relay terms,
// never in a vendor's wire terms.
//
// It became a real package when three concrete adapters needed the same
// contract — Codex (`harness/codex`), OpenCode (`harness/opencode`), and
// Devin (`harness/devin`). It is NOT a universal harness framework: it
// holds only the operations the control plane performs today, plus the
// command/result/error vocabulary those operations share. Vendor protocol
// DTOs stay inside the vendor package; the two ACP adapters share their
// protocol core in `harness/acp` because they speak one verified standard
// protocol, while Codex does not use it at all.
package harness

import (
	"context"
	"errors"

	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// Canonical adapter errors. HTTP mapping belongs to the daemon; adapters
// only classify the failure and may wrap these with vendor context.
var (
	// ErrBusy means the session already has an in-flight turn.
	ErrBusy = errors.New("session busy")
	// ErrNoActiveTurn means cancel was requested with no in-flight turn.
	ErrNoActiveTurn = errors.New("no active turn")
	// ErrNativeSessionLost means exact resume of the recorded native
	// session failed. Relay never substitutes a different native session.
	ErrNativeSessionLost = errors.New("native session unavailable")
	// ErrNoSuchInput means the requested-input ID is unknown or already
	// resolved.
	ErrNoSuchInput = errors.New("unknown requested input")
	// ErrRuntimeUnavailable means the harness runtime could not be started
	// or initialized.
	ErrRuntimeUnavailable = errors.New("harness runtime unavailable")
	// ErrUnsupported means the operation is not supported by this harness
	// (a capability mismatch, never an internal failure).
	ErrUnsupported = errors.New("unsupported operation")
	// ErrInvalidConfig means the requested model/mode is not acceptable
	// for this harness or was rejected natively.
	ErrInvalidConfig = errors.New("invalid config")
)

// Adapter is the daemon-facing contract of one native harness. Only the
// operations the control plane performs today are here: no speculative
// methods for functionality no adapter implements.
//
// Every method is safe for concurrent use. Adapter state is daemon-memory
// only: durable Relay metadata is mutated exclusively through the
// daemon-supplied Materialize hook (see Deps), under the session's MetaMu.
type Adapter interface {
	// Name is the harness identity persisted on RelaySession.Harness
	// ("codex", "devin", "opencode").
	Name() string
	// Track registers a managed session (create or restore).
	Track(m *session.Managed)
	// Prompt accepts a user prompt and submits it as a native turn.
	Prompt(ctx context.Context, m *session.Managed, cmd PromptCommand) (PromptResult, error)
	// Cancel interrupts the in-flight native turn.
	Cancel(ctx context.Context, m *session.Managed) error
	// AnswerInput resolves a native requested-input request by exact Relay
	// input ID. Adapters without a verified elicitation path return
	// ErrUnsupported.
	AnswerInput(ctx context.Context, m *session.Managed, inputID string, answers []InputAnswer) error
	// ApplyConfig applies an accepted model/mode change. A harness that
	// cannot apply the requested field returns ErrUnsupported (or
	// ErrInvalidConfig when the value is natively rejected).
	ApplyConfig(ctx context.Context, m *session.Managed, cmd ConfigCommand) error
	// StopSession releases adapter state for a session being deleted: an
	// in-flight turn is interrupted best-effort, unresolved native
	// requests are aborted durably, and the shared runtime keeps serving
	// its other sessions.
	StopSession(m *session.Managed)
	// OnRuntimeGone is the supervisor's runtime-removal notification,
	// delivered exactly once per runtime generation.
	OnRuntimeGone(key string, rt *runtime.Runtime, reason string)
	// Metrics returns the last-known in-memory metrics for a session
	// (nil when the harness reported none). Reading metrics never wakes a
	// runtime.
	Metrics(sessionID string) *SessionMetrics
}

// PromptCommand is one Relay prompt submission. It is a Relay operation,
// not a vendor wire message: an adapter maps it onto its own protocol.
type PromptCommand struct {
	Text   string
	Model  string
	Mode   string
	Effort string
}

// PromptResult reports the accepted native turn. NativeSessionID is the
// exact native session identity in force for that turn — it may be
// provisional (not yet durable) for a harness whose native identity only
// becomes resumable after a completed turn.
type PromptResult struct {
	TurnID          string `json:"turnId"`
	NativeSessionID string `json:"nativeSessionId"`
	RuntimeID       string `json:"runtimeId"`
}

// ConfigCommand is an accepted configuration change.
type ConfigCommand struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// InputAnswer is one answer to one requested-input question.
type InputAnswer struct {
	QuestionID string   `json:"questionId"`
	Answers    []string `json:"answers"`
}

// SessionUpdate is one durable metadata mutation requested by an adapter.
// Nil fields are left unchanged; BumpGeneration increments the session
// generation in the same atomic write.
type SessionUpdate struct {
	NativeSessionID *string
	Model           *string
	Mode            *string
	State           *string
	BumpGeneration  bool
	// TelemetrySeqWatermark durably reserves RepoDex telemetry sequence
	// space (internal/telemetry); used only by the seq allocator, never by
	// harness adapters directly.
	TelemetrySeqWatermark *uint64
	// TelemetryStartedEvent freezes the canonical seq0 bytes — written
	// once, never overwritten (internal/telemetry only).
	TelemetryStartedEvent string
}

// ValidNativeID reports whether a durable native session identity is
// plausible enough to hand to a harness. Every adapter validates with
// this: a malformed value is never sent natively and never substituted —
// it fails closed as ErrNativeSessionLost (Relay never guesses a native
// session).
func ValidNativeID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}
