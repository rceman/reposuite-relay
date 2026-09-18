package harness

// SessionMetrics is the canonical, optional session accounting shape shared
// by every adapter and exposed (in memory only) through SessionInfo.
//
// Every field is optional and every numeric field is a pointer: an absent
// measurement is omitted, never reported as zero. Relay never synthesizes
// a value the harness did not report — no invented quota, context limit,
// or cost.
type SessionMetrics struct {
	Model string `json:"model,omitempty"`
	Mode  string `json:"mode,omitempty"`
	// ContextUsed/ContextLimit are the native context window accounting
	// when the harness reports it.
	ContextUsed  *int64 `json:"contextUsed,omitempty"`
	ContextLimit *int64 `json:"contextLimit,omitempty"`
	// Last-turn token accounting.
	InputTokens     *int64 `json:"inputTokens,omitempty"`
	OutputTokens    *int64 `json:"outputTokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoningTokens,omitempty"`
	CacheTokens     *int64 `json:"cacheTokens,omitempty"`
	// Quota accounting, only where the harness reports it.
	QuotaUsed  *float64 `json:"quotaUsed,omitempty"`
	QuotaLimit *float64 `json:"quotaLimit,omitempty"`
	ResetAt    string   `json:"resetAt,omitempty"`
}

// Int64 returns a pointer to v — a helper for building optional metrics
// without a local variable at every call site.
func Int64(v int64) *int64 { return &v }
