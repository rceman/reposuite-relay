package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipePair wires a Conn to an in-process scripted peer: the test writes
// frames to `toClient` and reads what the Conn sends on `fromClient`.
type pipePair struct {
	conn *Conn

	toClient   *io.PipeWriter
	fromClient *io.PipeReader

	mu       sync.Mutex
	received []frame
	readErr  error
	done     chan struct{}
}

func newPipePair(t *testing.T) *pipePair {
	t.Helper()
	clientToPeerR, clientToPeerW := io.Pipe()
	peerToClientR, peerToClientW := io.Pipe()
	p := &pipePair{
		conn:       NewConn(clientToPeerW, peerToClientR),
		toClient:   peerToClientW,
		fromClient: clientToPeerR,
		done:       make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(clientToPeerR)
		for sc.Scan() {
			var f frame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				p.mu.Lock()
				p.readErr = err
				p.mu.Unlock()
				return
			}
			p.mu.Lock()
			p.received = append(p.received, f)
			p.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		p.conn.Close(nil)
		clientToPeerW.Close()
		peerToClientW.Close()
	})
	return p
}

func (p *pipePair) writeFrame(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.toClient.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (p *pipePair) frames() []frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]frame, len(p.received))
	copy(out, p.received)
	return out
}

// waitFrame waits for a frame with the given id (or any frame when id<0).
func (p *pipePair) waitFrame(t *testing.T, id int64) frame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range p.frames() {
			if id < 0 || (f.ID != nil && *f.ID == id) {
				return f
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("frame id=%d not received", id)
	return frame{}
}

// TestCallCorrelatesResponses: out-of-order responses still resolve the
// right call.
func TestCallCorrelatesResponses(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()

	var wg sync.WaitGroup
	results := map[string]InitializeResult{}
	var mu sync.Mutex
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			var res InitializeResult
			if err := p.conn.Call(context.Background(), "initialize",
				InitializeParams{ClientInfo: ClientInfo{
					Name:    name,
					Version: "0",
				}}, &res); err != nil {
				t.Errorf("call %s: %v", name, err)
				return
			}
			mu.Lock()
			results[name] = res
			mu.Unlock()
		}(name)
	}
	// Answer in reverse order of arrival.
	first := p.waitFrame(t, -1)
	var second frame
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fs := p.frames()
		if len(fs) >= 2 {
			second = fs[1]
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	p.writeFrame(t, map[string]any{"id": *second.ID, "result": map[string]any{"userAgent": "second"}})
	p.writeFrame(t, map[string]any{"id": *first.ID, "result": map[string]any{"userAgent": "first"}})
	wg.Wait()
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.UserAgent] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("responses mismatched: %+v", results)
	}
}

// TestCallErrorPropagates: a JSON-RPC error becomes a Go error.
func TestCallErrorPropagates(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	errCh := make(chan error, 1)
	go func() {
		var res InitializeResult
		errCh <- p.conn.Call(context.Background(), "initialize", struct{}{}, &res)
	}()
	f := p.waitFrame(t, -1)
	p.writeFrame(t, map[string]any{"id": *f.ID,
		"error": map[string]any{"code": -32000, "message": "boom"}})
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// TestNotificationDispatch: notifications reach the sink in order.
func TestNotificationDispatch(t *testing.T) {
	p := newPipePair(t)
	var mu sync.Mutex
	var got []string
	p.conn.SetNotificationHandler(func(method string, _ json.RawMessage) {
		mu.Lock()
		got = append(got, method)
		mu.Unlock()
	})
	done := make(chan struct{})
	go func() { p.conn.Start(); close(done) }()
	p.writeFrame(t, map[string]any{"method": "turn/started", "params": map[string]any{}})
	p.writeFrame(t, map[string]any{"method": "turn/completed", "params": map[string]any{}})
	p.toClient.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "turn/started" || got[1] != "turn/completed" {
		t.Fatalf("notifications = %v", got)
	}
}

// TestServerRequestDeferredResponse: a handler may defer, then Respond.
func TestServerRequestDeferredResponse(t *testing.T) {
	p := newPipePair(t)
	p.conn.SetHandler(MethodRequestUserInput, func(_ context.Context, id int64, _ json.RawMessage) (any, error) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = p.conn.Respond(id, ToolRequestUserInputResponse{
				Answers: map[string]ToolRequestUserInputAnswer{"q1": {Answers: []string{"alpha"}}}})
		}()
		return nil, ErrPendingResponse
	})
	go p.conn.Start()
	p.writeFrame(t, map[string]any{"id": int64(900), "method": MethodRequestUserInput,
		"params": map[string]any{"threadId": "th", "turnId": "tn", "itemId": "it",
			"isBlocking": true, "questions": []map[string]any{{"id": "q1"}}}})
	f := p.waitFrame(t, 900)
	if f.Error != nil {
		t.Fatalf("deferred handler answered with an error: %+v", f.Error)
	}
	var resp ToolRequestUserInputResponse
	if err := json.Unmarshal(f.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Answers["q1"].Answers; len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("answers = %+v", resp.Answers)
	}
}

// TestUnknownServerRequestAnswers: an unsupported server request gets an
// explicit error instead of leaving the harness waiting forever.
func TestUnknownServerRequestAnswers(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	p.writeFrame(t, map[string]any{"id": int64(7), "method": "something/else",
		"params": map[string]any{}})
	f := p.waitFrame(t, 7)
	if f.Error == nil {
		t.Fatal("unknown server request must be answered with an error")
	}
}

// TestMalformedFrameFailsClosed: a non-JSON line ends the connection and
// fails pending calls instead of being skipped.
func TestMalformedFrameFailsClosed(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	errCh := make(chan error, 1)
	go func() {
		var res InitializeResult
		errCh <- p.conn.Call(context.Background(), "initialize", struct{}{}, &res)
	}()
	p.waitFrame(t, -1)
	if _, err := p.toClient.Write([]byte("not json at all\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("err = %v, want ErrMalformedFrame", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call not failed after malformed frame")
	}
	if !errors.Is(p.conn.Err(), ErrMalformedFrame) {
		t.Fatalf("conn err = %v", p.conn.Err())
	}
}

// TestOversizedFrameFailsClosed: a frame above MaxFrameBytes ends the
// connection rather than allocating without bound.
func TestOversizedFrameFailsClosed(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	big := strings.Repeat("x", MaxFrameBytes+1024)
	// The write blocks once the scanner gives up — that is the behavior
	// under test, so it runs in its own goroutine.
	go func() {
		_, _ = p.toClient.Write([]byte(`{"method":"` + big + `"}` + "\n"))
	}()
	select {
	case <-p.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("oversized frame did not end the connection")
	}
	if !errors.Is(p.conn.Err(), ErrFrameTooLarge) {
		t.Fatalf("conn err = %v, want ErrFrameTooLarge", p.conn.Err())
	}
}

// TestEOFPropagatesToPendingCalls: process exit (EOF) fails in-flight
// calls instead of hanging them.
func TestEOFPropagatesToPendingCalls(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	errCh := make(chan error, 1)
	go func() {
		var res InitializeResult
		errCh <- p.conn.Call(context.Background(), "initialize", struct{}{}, &res)
	}()
	p.waitFrame(t, -1)
	p.toClient.Close() // the process died
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("err = %v, want ErrConnClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call not failed on EOF")
	}
}

// TestCallContextCancellation: a cancelled call returns promptly.
func TestCallContextCancellation(t *testing.T) {
	p := newPipePair(t)
	go p.conn.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var res InitializeResult
	err := p.conn.Call(ctx, "initialize", struct{}{}, &res)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	// A late response must not panic or block (it is dropped).
	f := p.waitFrame(t, -1)
	p.writeFrame(t, map[string]any{"id": *f.ID, "result": map[string]any{}})
	time.Sleep(20 * time.Millisecond)
}

// TestTailBufferBounds: stderr retention is bounded.
func TestTailBufferBounds(t *testing.T) {
	tb := NewTailBuffer(16)
	_, _ = tb.Write([]byte(strings.Repeat("a", 32)))
	_, _ = tb.Write([]byte("TAIL"))
	if got := tb.String(); len(got) != 16 || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("tail = %q", got)
	}
	if tb.Len() != 16 {
		t.Fatalf("len = %d", tb.Len())
	}
}
