package codex

import "github.com/rceman/reposuite-relay/internal/harness"

// --- vendor-specific event payloads ---------------------------------
// Canonical payloads (message.user, turn.*, harness.started, session.config)
// live in internal/api so every harness publishes one shape.
type inputQuestion struct {
	ID       string                    `json:"id"`
	Header   string                    `json:"header"`
	Question string                    `json:"question"`
	Options  []ToolRequestUserInputOpt `json:"options,omitempty"`
	IsOther  bool                      `json:"isOther,omitempty"`
	// IsSecret marks a question whose answer is a credential/secret: the
	// answer is forwarded to the native harness but never persisted in a
	// durable Relay record.
	IsSecret bool `json:"isSecret,omitempty"`
}

type inputRequestedPayload struct {
	InputID    string          `json:"inputId"`
	TurnID     string          `json:"turnId"`
	ItemID     string          `json:"itemId"`
	IsBlocking bool            `json:"isBlocking"`
	Questions  []inputQuestion `json:"questions"`
}

// inputResolvedPayload is the durable input.resolved record. Answers
// holds only non-secret question answers; a question the native request
// marked isSecret is listed in Redacted by question ID — its plaintext
// answer is never written to the durable transcript.
type inputResolvedPayload struct {
	InputID  string              `json:"inputId"`
	Answers  map[string][]string `json:"answers"`
	Redacted []string            `json:"redacted,omitempty"`
}

type inputAbortedPayload struct {
	InputID string `json:"inputId"`
	Reason  string `json:"reason"`
}

type metricsPayload struct {
	Kind string `json:"kind"`
	Metrics
	// The canonical projection travels with the vendor-rich accounting, so
	// a client can read harness-neutral fields without knowing Codex.
	harness.SessionMetrics
}

// --- runtime lifecycle ----------------------------------------------

// derefMetrics adapts the optional projection to the embedded value field.
func derefMetrics(m *harness.SessionMetrics) harness.SessionMetrics {
	if m == nil {
		return harness.SessionMetrics{}
	}
	return *m
}
