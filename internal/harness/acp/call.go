package acp

import (
	"context"
	"encoding/json"
	"fmt"
)

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
		if _, ok := c.pending[call.id]; ok {
			// The request went out, so a late response may still legally
			// arrive: mark it abandoned (bounded) rather than mistaking it
			// for protocol corruption — or for another call's response.
			delete(c.pending, call.id)
			if len(c.abandoned) >= maxAbandonedResponses {
				c.mu.Unlock()
				c.fail(fmt.Errorf("%w: abandoned response bound exceeded", ErrProtocolCorruption))
				return fmt.Errorf("%s: %w", call.method, ctx.Err())
			}
			c.abandoned[call.id] = struct{}{}
		}
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", call.method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%s: %w", call.method, c.Err())
	}
}
