package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// pipePair builds a bidirectional byte channel: returns (clientWrite,
// clientRead, serverWrite, serverRead).
func pipePair(t *testing.T) (*os.File, *os.File, *os.File, *os.File) {
	t.Helper()
	sr, cw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cw.Close()
		cr.Close()
		sw.Close()
		sr.Close()
	})
	return cw, cr, sw, sr
}

// peer is one side of a test connection plus the raw other side.
type peer struct {
	conn *Conn
	raw  *os.File
}

func newPeer(t *testing.T, in, out *os.File) *peer {
	t.Helper()
	conn := NewConn(out, in)
	return &peer{
		conn: conn,
		raw:  out,
	}
}

// readFrame decodes the next frame the peer wrote.
func readFrame(t *testing.T, r *os.File) map[string]any {
	t.Helper()
	buf := make([]byte, MaxFrameBytes)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf[:n], &m); err != nil {
		t.Fatalf("decode frame %q: %v", buf[:n], err)
	}
	return m
}

// TestRPCRequestResponse: a request is framed, correlated by ID, and decoded.
func TestRPCRequestResponse(t *testing.T) {
	cw, cr, sw, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	server := newPeer(t, sr, sw)

	go func() {
		m := readFrame(t, sr)
		if m["method"] != "ping" {
			t.Errorf("method = %v", m["method"])
		}
		id := int64(m["id"].(float64))
		_ = server.conn.Respond(id, map[string]any{"pong": true})
	}()
	go client.conn.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out struct {
		Pong bool `json:"pong"`
	}
	if err := client.conn.Call(ctx, "ping", map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Pong {
		t.Fatal("response not decoded")
	}
}

// TestRPCUnknownServerRequestFailsClosed: an agent→client request Relay does
// not implement gets an explicit JSON-RPC error, never silence.
func TestRPCUnknownServerRequestFailsClosed(t *testing.T) {
	cw, cr, sw, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	server := newPeer(t, sr, sw)
	go client.conn.Start()
	go server.conn.Start()

	call, err := server.conn.StartCall("fs/read_text_file", map[string]any{"path": "/etc/hostname"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = call.Wait(ctx, nil)
	if err == nil {
		t.Fatal("unimplemented request must fail")
	}
	if !strings.Contains(err.Error(), "unsupported method") {
		t.Fatalf("err = %v", err)
	}
}

// TestRPCMalformedFrameFailsClosed: a non-JSON line ends the connection and
// fails every pending call instead of desynchronizing.
func TestRPCMalformedFrameFailsClosed(t *testing.T) {
	cw, cr, sw, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	call, err := client.conn.StartCall("session/prompt", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(sw, "not json at all\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := call.Wait(ctx, nil); err == nil {
		t.Fatal("pending call must fail after a malformed frame")
	}
	if !errors.Is(client.conn.Err(), ErrMalformedFrame) {
		t.Fatalf("conn err = %v, want ErrMalformedFrame", client.conn.Err())
	}
}

// TestRPCOversizedFrameFailsClosed: a frame past MaxFrameBytes ends the
// connection; the scanner never grows without bound.
func TestRPCOversizedFrameFailsClosed(t *testing.T) {
	cw, cr, sw, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	call, err := client.conn.StartCall("session/prompt", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { fmt.Fprint(sw, strings.Repeat("x", MaxFrameBytes+8)+"\n") }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := call.Wait(ctx, nil); err == nil {
		t.Fatal("pending call must fail after an oversized frame")
	}
	err = client.conn.Err()
	if !errors.Is(err, ErrFrameTooLarge) && !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("conn err = %v, want a frame-bound failure", err)
	}
}

// TestRPCContextCancelDropsCall: a cancelled call is dropped, not leaked, and
// a late response is ignored.
func TestRPCContextCancelDropsCall(t *testing.T) {
	cw, cr, _, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	call, err := client.conn.StartCall("slow", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Wait(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	m := readFrame(t, sr)
	id := int64(m["id"].(float64))
	// A late response for an abandoned call must not panic or block.
	_ = client.conn.Respond(id, map[string]any{})
}

// TestRPCNotificationDispatch: notifications reach the sink in order.
func TestRPCNotificationDispatch(t *testing.T) {
	cw, cr, sw, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	got := make(chan string, 4)
	client.conn.SetNotificationHandler(func(method string, _ json.RawMessage) {
		got <- method
	})
	go client.conn.Start()
	for _, m := range []string{"session/update", "session/update", "other"} {
		if err := NewConn(sw, srDiscard{}).Notify(m, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"session/update", "session/update", "other"} {
		select {
		case m := <-got:
			if m != want {
				t.Fatalf("notification %q, want %q", m, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("notification not delivered")
		}
	}
}

// srDiscard is a writer-only transport half for notification tests.
type srDiscard struct{}

func (srDiscard) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestRPCDeferredHandler: a handler returning ErrPendingResponse may answer
// later (the permission path).
func TestRPCDeferredHandler(t *testing.T) {
	cw, cr, sw, sr := pipePair(t)
	client := newPeer(t, cr, cw)
	server := newPeer(t, sr, sw)
	go server.conn.Start()
	client.conn.SetHandler("session/request_permission", func(_ context.Context, id int64, _ json.RawMessage) (any, error) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = client.conn.Respond(id, map[string]any{"outcome": "selected"})
		}()
		return nil, ErrPendingResponse
	})
	go client.conn.Start()

	call, err := server.conn.StartCall("session/request_permission", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out map[string]any
	if err := call.Wait(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out["outcome"] != "selected" {
		t.Fatalf("outcome = %v", out["outcome"])
	}
}

// TestConnCloseFailsPending: ending the connection fails in-flight calls with
// the terminal reason.
func TestConnCloseFailsPending(t *testing.T) {
	cw, cr, _, _ := pipePair(t)
	client := newPeer(t, cr, cw)
	go client.conn.Start()
	call, err := client.conn.StartCall("session/prompt", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	client.conn.Close(ErrConnClosed)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := call.Wait(ctx, nil); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("err = %v, want ErrConnClosed", err)
	}
}
