package api

import "github.com/rceman/reposuite-relay/internal/harness"

// Canonical event payload shapes.
//
// An event has ONE wire shape regardless of which harness produced it: these
// types are shared by every adapter (Codex, Devin, OpenCode) so a client
// never needs harness-specific knowledge to read the canonical stream.
// Harness-specific detail stays inside the harness's own transient events.
type (
	// MessageUserPayload is the payload of message.user.
	MessageUserPayload struct {
		Text   string `json:"text"`
		Model  string `json:"model,omitempty"`
		Effort string `json:"effort,omitempty"`
	}

	// MessageCompletedPayload is the payload of message.agent.completed.
	MessageCompletedPayload struct {
		TurnID string `json:"turnId"`
		ItemID string `json:"itemId,omitempty"`
		Text   string `json:"text,omitempty"`
	}

	// TurnEventPayload is the payload of turn.failed and turn.interrupted.
	TurnEventPayload struct {
		TurnID string `json:"turnId,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	// HarnessStartedPayload is the payload of harness.started: a runtime
	// generation took ownership of the session's native session.
	HarnessStartedPayload struct {
		RuntimeID       string `json:"runtimeId"`
		NativeSessionID string `json:"nativeSessionId"`
		Model           string `json:"model,omitempty"`
		Resumed         bool   `json:"resumed"`
	}

	// RuntimeExitedPayload is the payload of runtime.exited.
	RuntimeExitedPayload struct {
		RuntimeID string `json:"runtimeId"`
		Reason    string `json:"reason,omitempty"`
	}

	// ConfigChangedPayload is the payload of session.config.
	ConfigChangedPayload struct {
		Model string `json:"model"`
		Mode  string `json:"mode"`
	}

	// SessionStatePayload is the payload of session.state.
	SessionStatePayload struct {
		State string `json:"state"`
	}

	// NativeSessionPayload is the payload of session.native: proven native
	// materialization.
	NativeSessionPayload struct {
		NativeSessionID string `json:"nativeSessionId"`
		Generation      int    `json:"generation"`
	}

	// HarnessErrorPayload is the payload of the transient harness.error.
	HarnessErrorPayload struct {
		Message string `json:"message"`
		// Fatal marks a harness-reported failure Relay could not classify.
		Fatal bool `json:"fatal,omitempty"`
	}

	// MetricsUpdatedPayload is the payload of the transient metrics.updated:
	// the canonical optional accounting plus the kind of update.
	MetricsUpdatedPayload struct {
		Kind string `json:"kind"`
		harness.SessionMetrics
	}
)
