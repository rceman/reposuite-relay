package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Client is the typed ACP protocol client over one connection. It owns no
// process and no durable state: the caller owns both.
type Client struct {
	conn *Conn

	mu   sync.Mutex
	caps InitializeResult
}

// NewClient wraps a connection.
func NewClient(conn *Conn) *Client { return &Client{conn: conn} }

// Conn exposes the underlying engine (handler wiring).
func (c *Client) Conn() *Conn { return c.conn }

// Capabilities returns the capabilities the runtime actually reported. It is
// empty until Initialize succeeded.
func (c *Client) Capabilities() InitializeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// Initialize performs the handshake and retains the reported capabilities.
// It must be the first call.
func (c *Client) Initialize(ctx context.Context, client ClientInfo) (InitializeResult, error) {
	var res InitializeResult
	err := c.conn.Call(ctx, MethodInitialize, InitializeParams{
		ClientCapabilities: ClientCapabilities{},
		ClientInfo:         &client,
		ProtocolVersion:    Version,
	}, &res)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.caps = res
	c.mu.Unlock()
	return res, nil
}

// SessionNew creates a native session and returns its exact identity plus
// the advertised config/model/mode surface.
func (c *Client) SessionNew(ctx context.Context, cwd string) (NewSessionResult, error) {
	var res NewSessionResult
	err := c.conn.Call(ctx, MethodSessionNew, NewSessionParams{
		Cwd:        cwd,
		McpServers: []MCPServer{},
	}, &res)
	if err != nil {
		return res, err
	}
	if res.SessionID == "" {
		return res, fmt.Errorf("%s returned no session id", MethodSessionNew)
	}
	return res, nil
}

// SessionLoad reattaches to an EXACT native session. It never substitutes a
// different session: a failure or an identity mismatch is the caller's
// NATIVE_SESSION_LOST.
func (c *Client) SessionLoad(ctx context.Context, cwd, sessionID string) (LoadSessionResult, error) {
	var res LoadSessionResult
	err := c.conn.Call(ctx, MethodSessionLoad, LoadSessionParams{
		Cwd:        cwd,
		McpServers: []MCPServer{},
		SessionID:  sessionID,
	}, &res)
	if err != nil {
		return res, err
	}
	if res.SessionID == "" {
		// ACP does not require session/load to echo the identity — the
		// request already named it (devin acp 3000.11.1 returns modes
		// and configOptions only). Normalize to the requested id.
		res.SessionID = sessionID
	} else if res.SessionID != sessionID {
		return res, fmt.Errorf("%s returned session %q, want the exact %q",
			MethodSessionLoad, res.SessionID, sessionID)
	}
	return res, nil
}

// StartPrompt submits one prompt and returns the in-flight call. The
// response IS the terminal turn result (stopReason + usage), so the caller
// awaits it asynchronously and keeps the turn in flight until it resolves.
func (c *Client) StartPrompt(params PromptParams) (*Call, error) {
	return c.conn.StartCall(MethodSessionPrompt, params)
}

// Prompt submits one prompt and waits for the terminal turn result.
func (c *Client) Prompt(ctx context.Context, params PromptParams) (PromptResult, error) {
	var res PromptResult
	call, err := c.StartPrompt(params)
	if err != nil {
		return res, err
	}
	if err := call.Wait(ctx, &res); err != nil {
		return res, err
	}
	return res, nil
}

// Cancel interrupts the in-flight turn. ACP spells cancel as a notification;
// the pending prompt call resolves with stopReason "cancelled".
func (c *Client) Cancel(sessionID string) error {
	return c.conn.Notify(MethodSessionCancel, CancelParams{SessionID: sessionID})
}

// SessionList enumerates native sessions. Relay uses it for diagnostics and
// capability evidence only — never to resolve an identity.
func (c *Client) SessionList(ctx context.Context, cwd string) (ListSessionsResult, error) {
	var res ListSessionsResult
	err := c.conn.Call(ctx, MethodSessionList, ListSessionsParams{Cwd: cwd}, &res)
	return res, err
}

// SetConfigOption applies one config option value and returns the updated
// option set the runtime reported.
func (c *Client) SetConfigOption(ctx context.Context, params SetConfigOptionParams) (SetConfigOptionResult, error) {
	var res SetConfigOptionResult
	err := c.conn.Call(ctx, MethodSessionSetConfigOption, params, &res)
	return res, err
}

// SetMode switches the native session mode (capability-gated; a runtime that
// does not implement it answers method-not-found, which the caller reports
// as an unsupported operation).
func (c *Client) SetMode(ctx context.Context, sessionID, modeID string) error {
	return c.conn.Call(ctx, MethodSessionSetMode, SetModeParams{
		ModeID:    modeID,
		SessionID: sessionID,
	}, nil)
}

// Respond answers a deferred agent→client request.
func (c *Client) Respond(id int64, result any) error { return c.conn.Respond(id, result) }

// DecodeUpdate decodes a `session/update` notification payload.
func DecodeUpdate(params json.RawMessage) (SessionUpdateNotification, error) {
	var n SessionUpdateNotification
	err := json.Unmarshal(params, &n)
	return n, err
}
