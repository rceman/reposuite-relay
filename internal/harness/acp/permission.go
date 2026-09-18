package acp

import (
	"encoding/json"
	"strings"
)

// PermissionOption is one advertised approval choice. optionId is an opaque
// runtime-chosen string (OpenCode uses "once"/"always"/"reject"; Devin uses
// "allow_once"/"allow_always_global"/"switch_bypass"/…), so the KIND is the
// only portable semantic and the ID/name are heuristics, never assumptions.
type PermissionOption struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	OptionID string `json:"optionId"`
}

// RequestPermissionParams is the agent→client approval request. It is an
// approval mechanism, NOT Relay's requested-input surface: Relay never turns
// one into a durable input.requested event.
type RequestPermissionParams struct {
	Options   []PermissionOption `json:"options"`
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall,omitempty"`
}

// PermissionOutcome is the decision Relay reports back.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// RequestPermissionResult wraps the outcome (the verified nesting:
// {"outcome":{"outcome":"selected","optionId":"…"}}).
type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// Selected builds a "selected" result for one option.
func Selected(optionID string) RequestPermissionResult {
	return RequestPermissionResult{Outcome: PermissionOutcome{
		Outcome:  OutcomeSelected,
		OptionID: optionID,
	}}
}

// Cancelled builds a fail-closed "cancelled" result.
func Cancelled() RequestPermissionResult {
	return RequestPermissionResult{Outcome: PermissionOutcome{
		Outcome: OutcomeCancelled,
	}}
}

// SelectAllowOption chooses the option Relay answers an approval request with
// when the harness is configured for full/bypass permissions.
//
// Order: the strongest advertised allow kind first (allow_always, then
// allow_once) — never a reject kind and never blindly index 0. When the
// runtime advertises no recognizable allow option the decision FAILS CLOSED
// (ok=false) and the caller cancels rather than approving something it could
// not classify.
func SelectAllowOption(options []PermissionOption) (PermissionOption, bool) {
	for _, kind := range []string{KindAllowAlways, KindAllowOnce} {
		for _, o := range options {
			if o.Kind == kind {
				return o, true
			}
		}
	}
	// Vendors may omit or extend the kind enum; fall back to an explicit
	// semantic match on the option ID and label, still refusing anything
	// that reads as a rejection.
	for _, o := range options {
		if isRejectLike(o) || !isAllowLike(o) {
			continue
		}
		return o, true
	}
	return PermissionOption{}, false
}

// rejectWords mark an option as a refusal even if it also looks permissive.
var rejectWords = []string{"reject", "deny", "decline", "no ", "never", "cancel", "skip"}

// allowWords mark an option as an approval.
var allowWords = []string{"allow", "approve", "accept", "yes", "always", "once", "bypass", "grant", "permit"}

func isRejectLike(o PermissionOption) bool {
	switch o.Kind {
	case KindRejectOnce, KindRejectAlways:
		return true
	}
	return matchesAny(o, rejectWords)
}

func isAllowLike(o PermissionOption) bool { return matchesAny(o, allowWords) }

func matchesAny(o PermissionOption, words []string) bool {
	hay := strings.ToLower(o.OptionID + " " + o.Name + " " + o.Kind)
	for _, w := range words {
		if strings.Contains(hay, w) {
			return true
		}
	}
	return false
}
