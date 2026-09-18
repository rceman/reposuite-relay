package codex

// --- event payloads -------------------------------------------------
type messageUserPayload struct {
	Text   string `json:"text"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type messageCompletedPayload struct {
	TurnID string `json:"turnId"`
	ItemID string `json:"itemId,omitempty"`
	Text   string `json:"text,omitempty"`
}

type turnEventPayload struct {
	TurnID string `json:"turnId,omitempty"`
	Error  string `json:"error,omitempty"`
}

type runtimeExitedPayload struct {
	RuntimeID string `json:"runtimeId"`
	Reason    string `json:"reason,omitempty"`
}

type harnessStartedPayload struct {
	RuntimeID       string `json:"runtimeId"`
	NativeSessionID string `json:"nativeSessionId"`
	Model           string `json:"model,omitempty"`
	Resumed         bool   `json:"resumed"`
}

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

type nativeSessionPayload struct {
	NativeSessionID string `json:"nativeSessionId"`
	Generation      int    `json:"generation"`
}

type metricsPayload struct {
	Kind string `json:"kind"`
	Metrics
}

// --- runtime lifecycle ----------------------------------------------
