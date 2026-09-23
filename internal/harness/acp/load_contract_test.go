// session/load identity-contract coverage at the shared ACP client.

package acp

import (
	"context"
	"testing"
	"time"
)

// TestFakeLoadErrorIsNotSubstituted: a failed exact load stays a failure; the
// fake never silently returns a different session.
func TestFakeLoadErrorIsNotSubstituted(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeLoadError)
	h.init(t)
	cwd := t.TempDir()
	sess := h.newSession(t, cwd)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.client.SessionLoad(ctx, cwd, sess.SessionID); err == nil {
		t.Fatal("load must fail in load-error mode")
	}
}

// TestSessionLoadIdentityContract covers the shared ACP wire rule for the
// response identity: an absent echo normalizes to the requested id (the
// request named the load target — real devin acp 3000.11.1 behavior), a
// matching echo is accepted, and a non-empty DIFFERENT id is a hard
// mismatch — never a substitution.
func TestSessionLoadIdentityContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{name: "absent echo normalizes to requested", mode: FakeLoadNoEcho},
		{name: "matching echo accepted", mode: FakeHappy},
		{name: "mismatched echo rejected", mode: FakeLoadMismatch, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startFake(t, FakeVendorDevin, tc.mode)
			h.init(t)
			cwd := t.TempDir()
			sess := h.newSession(t, cwd)
			h.prompt(t, sess.SessionID, "materialize") // Devin needs a proven turn
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := h.client.SessionLoad(ctx, cwd, sess.SessionID)
			if tc.wantErr {
				if err == nil {
					t.Fatal("mismatched sessionId must fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("load failed: %v", err)
			}
			if res.SessionID != sess.SessionID {
				t.Fatalf("SessionID = %q, want the exact requested %q", res.SessionID, sess.SessionID)
			}
		})
	}
}
