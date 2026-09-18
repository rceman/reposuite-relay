package acp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestFakeUnknownRequestGetsErrorResponse: Relay answers an unimplemented
// agent request with an explicit error, and the fake observes it.
func TestFakeUnknownRequestGetsErrorResponse(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeUnknownRequest)
	h.init(t)
	sess := h.newSession(t, t.TempDir())
	h.prompt(t, sess.SessionID, "hi")
	joined := strings.Join(ReadFakeEvents(h.stateDir), "\n")
	if !strings.Contains(joined, "unsupported method fs/read_text_file") {
		t.Fatalf("events = %v", ReadFakeEvents(h.stateDir))
	}
}

// TestFakeMalformedFrameEndsConnection: a garbage frame kills the connection
// rather than being skipped.
func TestFakeMalformedFrameEndsConnection(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeMalformed)
	h.init(t)
	sess := h.newSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.client.Prompt(ctx, PromptParams{
		Prompt:    []ContentBlock{TextBlock("boom")},
		SessionID: sess.SessionID,
	})
	if err == nil {
		t.Fatal("malformed frame must fail the prompt")
	}
	if !errors.Is(h.conn.Err(), ErrMalformedFrame) {
		t.Fatalf("conn err = %v", h.conn.Err())
	}
}

// TestFakeOversizedFrameEndsConnection: the frame bound is enforced on the
// ACP transport too.
func TestFakeOversizedFrameEndsConnection(t *testing.T) {
	h := startFake(t, FakeVendorDevin, FakeOversized)
	h.init(t)
	sess := h.newSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := h.client.Prompt(ctx, PromptParams{
		Prompt:    []ContentBlock{TextBlock("boom")},
		SessionID: sess.SessionID,
	}); err == nil {
		t.Fatal("oversized frame must fail the prompt")
	}
	if err := h.conn.Err(); err == nil {
		t.Fatal("connection must end")
	}
}
