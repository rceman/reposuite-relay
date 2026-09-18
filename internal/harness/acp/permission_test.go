package acp

import "testing"

func kindOf(o PermissionOption) string { return o.Kind }

// TestSelectAllowOptionPrefersStrongestAllow: the strongest advertised allow
// kind wins, and a reject is never selected.
func TestSelectAllowOptionPrefersStrongestAllow(t *testing.T) {
	options := []PermissionOption{
		{Kind: KindRejectAlways, Name: "Reject", OptionID: "reject"},
		{Kind: KindAllowOnce, Name: "Allow once", OptionID: "once"},
		{Kind: KindAllowAlways, Name: "Always allow", OptionID: "always"},
	}
	got, ok := SelectAllowOption(options)
	if !ok {
		t.Fatal("expected an allow option")
	}
	if kindOf(got) != KindAllowAlways || got.OptionID != "always" {
		t.Fatalf("selected %+v, want the allow_always option", got)
	}
}

// TestSelectAllowOptionOpenCodeIDs: OpenCode advertises optionId
// "once"/"always"/"reject" with standard kinds.
func TestSelectAllowOptionOpenCodeIDs(t *testing.T) {
	got, ok := SelectAllowOption([]PermissionOption{
		{OptionID: "once", Kind: KindAllowOnce, Name: "Allow once"},
		{OptionID: "always", Kind: KindAllowAlways, Name: "Always allow"},
		{OptionID: "reject", Kind: KindRejectOnce, Name: "Reject"},
	})
	if !ok || got.OptionID != "always" {
		t.Fatalf("selected %+v ok=%v", got, ok)
	}
}

// TestSelectAllowOptionVendorIDs: Devin's option IDs are vendor-chosen; the
// kind still decides.
func TestSelectAllowOptionVendorIDs(t *testing.T) {
	got, ok := SelectAllowOption([]PermissionOption{
		{OptionID: "switch_bypass", Kind: KindAllowAlways, Name: "Yes, switch to bypass mode"},
		{OptionID: "reject_once", Kind: KindRejectOnce, Name: "Reject"},
	})
	if !ok || got.OptionID != "switch_bypass" {
		t.Fatalf("selected %+v ok=%v", got, ok)
	}
}

// TestSelectAllowOptionFailsClosed: with no recognizable allow option the
// decision fails closed instead of approving by position.
func TestSelectAllowOptionFailsClosed(t *testing.T) {
	for _, options := range [][]PermissionOption{
		nil,
		{},
		{{OptionID: "escalate-42", Kind: "escalate", Name: "Escalate to a human"}},
		{{OptionID: "reject", Kind: KindRejectOnce, Name: "Reject"}},
		{{OptionID: "no-way", Name: "No, never do this"}},
	} {
		if got, ok := SelectAllowOption(options); ok {
			t.Fatalf("options %+v selected %+v, want fail-closed", options, got)
		}
	}
}

// TestSelectAllowOptionSemanticFallback: an extended kind enum still resolves
// through an explicit allow-like ID/label.
func TestSelectAllowOptionSemanticFallback(t *testing.T) {
	got, ok := SelectAllowOption([]PermissionOption{
		{OptionID: "allow_all_fetches", Kind: "extended_kind", Name: "Yes, always allow all web fetches"},
		{OptionID: "reject_once", Kind: KindRejectOnce, Name: "Reject"},
	})
	if !ok || got.OptionID != "allow_all_fetches" {
		t.Fatalf("selected %+v ok=%v", got, ok)
	}
}

// TestPermissionOutcomeShapes: the verified nesting is {"outcome":{...}}.
func TestPermissionOutcomeShapes(t *testing.T) {
	sel := Selected("once")
	if sel.Outcome.Outcome != OutcomeSelected || sel.Outcome.OptionID != "once" {
		t.Fatalf("selected = %+v", sel)
	}
	can := Cancelled()
	if can.Outcome.Outcome != OutcomeCancelled || can.Outcome.OptionID != "" {
		t.Fatalf("cancelled = %+v", can)
	}
}

// TestSelectAllowOptionCancelledIsNotAnAllow: an ACP "cancelled" outcome is
// not an approval.
func TestSelectAllowOptionCancelledIsNotAnAllow(t *testing.T) {
	if got, ok := SelectAllowOption([]PermissionOption{{OptionID: "cancel", Name: "Cancel"}}); ok {
		t.Fatalf("selected %+v, want fail-closed", got)
	}
}
