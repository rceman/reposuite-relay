package client

import (
	"context"
	"github.com/rceman/reposuite-relay/internal/api"
	"net/http"
	"net/url"
	"strconv"
)

func sessionPath(key string) string { return "/v1/sessions/" + url.PathEscape(key) }

// --- typed methods ----------------------------------------------------

// DaemonInfo returns the daemon generation info.
func (c *Client) DaemonInfo(ctx context.Context) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodGet, "/v1/daemon", nil, &resp)
	return resp.Daemon, err
}

// Sessions lists durable sessions (COLD sessions included).
func (c *Client) Sessions(ctx context.Context) (api.SessionList, error) {
	var resp api.SessionList
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &resp)
	return resp, err
}

// ServeFixture creates a fixture session.
func (c *Client) ServeFixture(ctx context.Context, key, cwd string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/sessions/fixture",
		api.FixtureRequest{Key: key, Cwd: cwd}, &resp)
	return resp, err
}

// CreateSession creates a durable session for one built-in harness (COLD —
// no harness process is started until the first prompt). The harness name
// selects the creation route; it is never an executable or an argument.
func (c *Client) CreateSession(ctx context.Context, harness, key, cwd, model, mode string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+harness,
		api.CreateRequest{Key: key, Cwd: cwd, Model: model, Mode: mode}, &resp)
	return resp, err
}

// Prompt submits a user prompt as a native turn (202 Accepted).
func (c *Client) Prompt(ctx context.Context, key, text, model, effort string) (api.PromptResponse, error) {
	var resp api.PromptResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/prompt",
		api.PromptRequest{Text: text, Model: model, Effort: effort}, &resp)
	return resp, err
}

// Cancel interrupts the in-flight native turn.
func (c *Client) Cancel(ctx context.Context, key string) (api.CancelResponse, error) {
	var resp api.CancelResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/cancel", nil, &resp)
	return resp, err
}

// AnswerInput answers a native requested-input request.
func (c *Client) AnswerInput(ctx context.Context, key, inputID string, answers []api.InputAnswer) (api.InputResponse, error) {
	var resp api.InputResponse
	err := c.do(ctx, http.MethodPost, sessionPath(key)+"/input",
		api.InputRequest{InputID: inputID, Answers: answers}, &resp)
	return resp, err
}

// SetConfig accepts a model/mode change for a Codex session.
func (c *Client) SetConfig(ctx context.Context, key string, model, mode *string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodPatch, sessionPath(key)+"/config",
		api.ConfigRequest{Model: model, Mode: mode}, &resp)
	return resp, err
}

// Status returns one session.
func (c *Client) Status(ctx context.Context, key string) (api.SessionResponse, error) {
	var resp api.SessionResponse
	err := c.do(ctx, http.MethodGet, sessionPath(key), nil, &resp)
	return resp, err
}

// StopSession deletes a session (runtime stop + durable delete).
func (c *Client) StopSession(ctx context.Context, key string) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodDelete, sessionPath(key), nil, &resp)
	return resp.Daemon, err
}

// Shutdown asks the daemon to stop gracefully.
func (c *Client) Shutdown(ctx context.Context) (api.DaemonInfo, error) {
	var resp api.DaemonResponse
	err := c.do(ctx, http.MethodPost, "/v1/daemon/shutdown", nil, &resp)
	return resp.Daemon, err
}

// Transcript fetches the bounded durable transcript tail. The limit is
// always sent explicitly: limit 0 means "no records", and the server
// rejects negative limits.
func (c *Client) Transcript(ctx context.Context, key string, limit int) (api.TranscriptPage, error) {
	var resp api.TranscriptPage
	path := sessionPath(key) + "/transcript?limit=" + strconv.Itoa(limit)
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}
