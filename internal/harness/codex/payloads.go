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
}

type inputRequestedPayload struct {
	InputID    string          `json:"inputId"`
	TurnID     string          `json:"turnId"`
	ItemID     string          `json:"itemId"`
	IsBlocking bool            `json:"isBlocking"`
	Questions  []inputQuestion `json:"questions"`
}

type inputResolvedPayload struct {
	InputID string              `json:"inputId"`
	Answers map[string][]string `json:"answers"`
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
