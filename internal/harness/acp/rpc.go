package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Stable transport errors.
var (
	// ErrConnClosed means the connection ended (process exit, malformed
	// frame, oversized frame) — pending calls fail with this wrapped.
	ErrConnClosed = errors.New("acp connection closed")
	// ErrMalformedFrame means a line was not a valid JSON-RPC message: the
	// connection fails closed rather than guessing.
	ErrMalformedFrame = errors.New("malformed acp frame")
	// ErrFrameTooLarge means a frame exceeded MaxFrameBytes.
	ErrFrameTooLarge = errors.New("acp frame too large")
	// ErrPendingResponse is returned by a server-request handler that will
	// respond later (deferred permission decisions).
	ErrPendingResponse = errors.New("response pending")
)

// Handler handles one agent→client request. The request ID is supplied so a
// handler may defer its response and answer later through Conn.Respond.
// Returning ErrPendingResponse defers; any other error is sent back as a
// JSON-RPC error.
type Handler func(ctx context.Context, id int64, params json.RawMessage) (any, error)

// rpcMessage is the union of every frame shape.
type rpcMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("acp rpc error %d", e.Code)
	}
	return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message)
}

// Conn is a line-delimited JSON-RPC 2.0 engine over one ACP stdio
// transport: request IDs, out-of-order responses, notifications,
// agent→client requests, serialized writes, exactly one stdout reader,
// pending-call failure on connection end, and context cancellation.
//
// It is symmetric — the adapter uses it as the client, the deterministic
// fake uses the same type as the agent — and deliberately not a generic
// multi-vendor RPC framework.
type Conn struct {
	stdin io.Writer

	writeMu sync.Mutex

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]*Call
	handlers map[string]Handler
	onNotify func(method string, params json.RawMessage)
	closed   bool
	closeErr error

	scanner *bufio.Scanner
	done    chan struct{}
}

// NewConn builds a connection over the given transport halves. The caller
// owns process lifecycle; Close only ends RPC correlation.
func NewConn(stdin io.Writer, stdout io.Reader) *Conn {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	return &Conn{
		stdin:    stdin,
		scanner:  sc,
		pending:  map[int64]*Call{},
		handlers: map[string]Handler{},
		done:     make(chan struct{}),
	}
}

// SetNotificationHandler installs the notification sink. Notifications are
// dispatched synchronously on the reader goroutine — deterministic ordering,
// no unbounded goroutine creation.
func (c *Conn) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onNotify = fn
}

// SetHandler installs the handler for one agent→client request method.
func (c *Conn) SetHandler(method string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[method] = h
}

// Done closes when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (nil while live).
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Start runs the stdout reader until EOF or a fatal frame error. It returns
// the terminal error (nil on clean EOF). Exactly one caller.
func (c *Conn) Start() error {
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > MaxFrameBytes {
			c.fail(fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(line)))
			return c.Err()
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			c.fail(fmt.Errorf("%w: %v", ErrMalformedFrame, err))
			return c.Err()
		}
		switch {
		case m.Method != "" && m.ID != nil:
			c.dispatchServerRequest(*m.ID, m)
		case m.Method != "":
			c.dispatchNotification(m)
		case m.ID != nil:
			c.deliver(*m.ID, m)
		default:
			c.fail(fmt.Errorf("%w: frame has neither id nor method", ErrMalformedFrame))
			return c.Err()
		}
	}
	if err := c.scanner.Err(); err != nil {
		c.fail(fmt.Errorf("%w: %v", ErrFrameTooLarge, err))
		return c.Err()
	}
	c.fail(ErrConnClosed) // EOF: the process exited
	return c.Err()
}

func (c *Conn) dispatchNotification(m rpcMessage) {
	c.mu.Lock()
	fn := c.onNotify
	c.mu.Unlock()
	if fn != nil {
		fn(m.Method, m.Params)
	}
}

func (c *Conn) dispatchServerRequest(id int64, m rpcMessage) {
	c.mu.Lock()
	h := c.handlers[m.Method]
	c.mu.Unlock()
	if h == nil {
		// Unknown agent request: answer with an explicit error rather than
		// leaving the harness waiting forever. Protocol integrity is kept.
		_ = c.RespondError(id, codeMethodNotFound, "unsupported method "+m.Method)
		return
	}
	res, err := h(context.Background(), id, m.Params)
	if errors.Is(err, ErrPendingResponse) {
		return // deferred: the caller responds later
	}
	if err != nil {
		_ = c.RespondError(id, codeServerError, err.Error())
		return
	}
	_ = c.Respond(id, res)
}

func (c *Conn) deliver(id int64, m rpcMessage) {
	c.mu.Lock()
	call, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return // response to an abandoned call (caller cancelled)
	}
	call.ch <- m
}

// Call sends one request and waits for its response or ctx/conn end.
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	call, err := c.StartCall(method, params)
	if err != nil {
		return err
	}
	return call.Wait(ctx, out)
}

// Call is one in-flight request whose response may arrive arbitrarily later
// (an ACP prompt resolves only when the turn ends).
type Call struct {
	conn   *Conn
	id     int64
	method string
	ch     chan rpcMessage
}

// StartCall writes one request and returns a handle to await. It returns
// once the frame is written, so a caller can report "accepted" without
// blocking on a turn.
func (c *Conn) StartCall(method string, params any) (*Call, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal %s params: %w", method, err)
	}
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	call := &Call{
		conn:   c,
		id:     id,
		method: method,
		ch:     make(chan rpcMessage, 1),
	}
	c.pending[id] = call
	c.mu.Unlock()

	frame := struct {
		Jsonrpc string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	if err := c.write(frame); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	return call, nil
}

// Wait awaits the call's response. A terminal response error, ctx
// cancellation, or connection end all fail the call.
func (call *Call) Wait(ctx context.Context, out any) error {
	c := call.conn
	select {
	case m := <-call.ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %w", call.method, m.Error)
		}
		if out != nil {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", call.method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, call.id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", call.method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s: %w", call.method, c.Err())
	}
}

// Notify sends a notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	frame := struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
	}
	return c.write(frame)
}

// Respond answers an agent→client request (deferred handlers).
func (c *Conn) Respond(id int64, result any) error {
	frame := struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Result  any    `json:"result,omitempty"`
	}{
		Jsonrpc: "2.0",
		ID:      id,
		Result:  result,
	}
	return c.write(frame)
}

// RespondError answers an agent→client request with a JSON-RPC error.
func (c *Conn) RespondError(id int64, code int, msg string) error {
	frame := struct {
		Jsonrpc string    `json:"jsonrpc"`
		ID      int64     `json:"id"`
		Error   *rpcError `json:"error"`
	}{
		Jsonrpc: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: msg,
		},
	}
	return c.write(frame)
}

// write serializes one frame; concurrent writers never interleave.
func (c *Conn) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.Err()
	}
	_, err = c.stdin.Write(raw)
	return err
}

// fail ends the connection once, failing every pending call.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.closeErr == nil {
		c.closeErr = err
	}
	// Pending calls are not answered: they observe the closed connection
	// through Done/Err, so the terminal reason is the connection's own.
	c.pending = map[int64]*Call{}
	c.mu.Unlock()
	close(c.done)
}

// Close ends the connection with the given reason (process exit, shutdown).
func (c *Conn) Close(err error) {
	if err == nil {
		err = ErrConnClosed
	}
	c.fail(err)
}
