package acp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// abandonedLen reports the live abandoned-id set size (same-package seam).
func abandonedLen(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.abandoned)
}

// waitFor polls a condition with a bounded deadline — never a timing sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRPCNeverIssuedResponseIDFailsClosed: a response for an id Relay never
// sent is protocol corruption — never mistaken for an abandoned call — and
// the connection fails closed with the pending call failing too.
func TestRPCNeverIssuedResponseIDFailsClosed(t *testing.T) {
	cw, cr, sw, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	call, err := client.conn.StartCall("session/prompt", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// The peer answers a request id that was never issued.
	fmt.Fprint(sw, `{"jsonrpc":"2.0","id":999,"result":{}}`+"\n")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = call.Wait(ctx, nil)
	if !errors.Is(err, ErrProtocolCorruption) {
		t.Fatalf("call err = %v, want ErrProtocolCorruption", err)
	}
	if !errors.Is(client.conn.Err(), ErrProtocolCorruption) {
		t.Fatalf("conn err = %v, want ErrProtocolCorruption", client.conn.Err())
	}
}

// TestRPCDuplicateResponseFailsClosed: a second response to an
// already-completed call is corruption — not a stray abandoned response.
func TestRPCDuplicateResponseFailsClosed(t *testing.T) {
	cw, cr, sw, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	call, err := client.conn.StartCall("ping", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := readFrame(t, sr)
	id := int64(m["id"].(float64))
	fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"result":{}}`+"\n", id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := call.Wait(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// A duplicate response to the completed call: corruption.
	fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"result":{}}`+"\n", id)
	waitFor(t, "connection fails closed", func() bool { return client.conn.Err() != nil })
	if !errors.Is(client.conn.Err(), ErrProtocolCorruption) {
		t.Fatalf("conn err = %v, want ErrProtocolCorruption", client.conn.Err())
	}
}

// TestRPCLateResponseToAbandonedCallIsConsumedOnce: a caller that gave up
// leaves a bounded marker; the late response is consumed and the marker is
// gone — a second response for the same id is then corruption.
func TestRPCLateResponseToAbandonedCallIsConsumedOnce(t *testing.T) {
	cw, cr, sw, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	call, err := client.conn.StartCall("slow", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := readFrame(t, sr)
	id := int64(m["id"].(float64))
	// The caller gives up before any response arrives.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = call.Wait(ctx, nil)
	if n := abandonedLen(client.conn); n != 1 {
		t.Fatalf("abandoned set = %d, want 1", n)
	}
	// The late response is legal: consumed once, connection stays healthy.
	fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"result":{}}`+"\n", id)
	waitFor(t, "abandoned marker consumed", func() bool { return abandonedLen(client.conn) == 0 })
	if client.conn.Err() != nil {
		t.Fatalf("late abandoned response must not kill the conn: %v", client.conn.Err())
	}
	// A second response for the same id is no longer legal.
	fmt.Fprintf(sw, `{"jsonrpc":"2.0","id":%d,"result":{}}`+"\n", id)
	waitFor(t, "connection fails closed", func() bool { return client.conn.Err() != nil })
	if !errors.Is(client.conn.Err(), ErrProtocolCorruption) {
		t.Fatalf("conn err = %v, want ErrProtocolCorruption", client.conn.Err())
	}
}

// TestRPCAbandonedBoundIsEnforced: the abandoned-id set is bounded — a peer
// that never answers cancelled calls cannot grow it forever; exhausting the
// bound fails the connection closed.
func TestRPCAbandonedBoundIsEnforced(t *testing.T) {
	cw, cr, _, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	abandon := func() {
		call, err := client.conn.StartCall("slow", map[string]any{})
		if err != nil {
			t.Fatalf("StartCall: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = call.Wait(ctx, nil)
	}
	for i := 0; i < maxAbandonedResponses; i++ {
		abandon()
	}
	if client.conn.Err() != nil {
		t.Fatalf("conn died before the bound: %v", client.conn.Err())
	}
	if n := abandonedLen(client.conn); n != maxAbandonedResponses {
		t.Fatalf("abandoned set = %d, want %d", n, maxAbandonedResponses)
	}
	// The next abandonment exceeds the bound: fail closed.
	abandon()
	if !errors.Is(client.conn.Err(), ErrProtocolCorruption) {
		t.Fatalf("conn err = %v, want ErrProtocolCorruption", client.conn.Err())
	}
}
