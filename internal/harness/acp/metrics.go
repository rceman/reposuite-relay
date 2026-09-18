package acp

import "github.com/rceman/reposuite-relay/internal/harness"

// Canonical projections of ACP measurements onto the harness-neutral optional
// shape. They live here because they are pure protocol-family projections: a
// measurement the runtime did not report stays ABSENT, never zero.

// ContextMetrics projects a `usage_update` notification (the native context
// window accounting) onto the canonical shape. A missing measurement stays
// absent; a nil result means the update carried nothing to report.
func ContextMetrics(u Update) *harness.SessionMetrics {
	if u.Used == nil && u.Size == nil {
		return nil
	}
	out := &harness.SessionMetrics{}
	if u.Used != nil {
		out.ContextUsed = harness.Int64(*u.Used)
	}
	if u.Size != nil {
		out.ContextLimit = harness.Int64(*u.Size)
	}
	return out
}

// UsageMetrics projects a prompt result's token accounting. The ACP usage
// shape is a union of optional fields, so an absent counter is never reported
// as a real zero.
func UsageMetrics(u *Usage) *harness.SessionMetrics {
	if u == nil {
		return nil
	}
	out := &harness.SessionMetrics{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		ReasoningTokens: u.ThoughtTokens,
	}
	if u.CachedReadTokens != nil {
		out.CacheTokens = u.CachedReadTokens
	} else if u.CachedWriteTokens != nil {
		out.CacheTokens = u.CachedWriteTokens
	}
	return out
}

// MergeMetrics overlays non-nil measurements onto the last-known set, so a
// later partial update never erases a field an earlier one reported.
func MergeMetrics(base, next *harness.SessionMetrics) *harness.SessionMetrics {
	if base == nil {
		return next
	}
	if next == nil {
		return base
	}
	out := *base
	if next.Model != "" {
		out.Model = next.Model
	}
	if next.Mode != "" {
		out.Mode = next.Mode
	}
	if next.ContextUsed != nil {
		out.ContextUsed = next.ContextUsed
	}
	if next.ContextLimit != nil {
		out.ContextLimit = next.ContextLimit
	}
	if next.InputTokens != nil {
		out.InputTokens = next.InputTokens
	}
	if next.OutputTokens != nil {
		out.OutputTokens = next.OutputTokens
	}
	if next.ReasoningTokens != nil {
		out.ReasoningTokens = next.ReasoningTokens
	}
	if next.CacheTokens != nil {
		out.CacheTokens = next.CacheTokens
	}
	if next.QuotaUsed != nil {
		out.QuotaUsed = next.QuotaUsed
	}
	if next.QuotaLimit != nil {
		out.QuotaLimit = next.QuotaLimit
	}
	if next.ResetAt != "" {
		out.ResetAt = next.ResetAt
	}
	return &out
}
