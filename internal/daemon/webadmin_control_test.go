// Browser control-plane integration: the Web Admin's session lifecycle
// (create, detail, prompt, cancel, input, config, delete) through the
// admin cookie + Origin + CSRF domain — exactly what the SPA sends.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
)

// webDaemonCodex starts a daemon with the deterministic fake app-server
// and a browser-like client (cookie jar, no redirects, bounded).
func webDaemonCodex(t *testing.T, fakeMode string) (*Daemon, paths.Paths, *http.Client) {
	t.Helper()
	if fakeMode != "" {
		t.Setenv("FAKE_CODEX_MODE", fakeMode)
	}
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		CodexCommand: func() (codex.Command, error) {
			return codex.Command{Path: testBinary(), Args: []string{"__fake-codex"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, p, &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// daemonClient returns the descriptor-authenticated machine client —
// used by tests for server-authoritative assertions, never by the SPA.
func daemonClient(t *testing.T, p paths.Paths) *client.Client {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon client did not become ready")
	return nil
}

// cookieJSON is the browser-control-plane request: admin cookie, exact
// canonical Origin, session CSRF token, JSON body — what the SPA sends
// for every unsafe /v1 call.
func cookieJSON(t *testing.T, c *http.Client, method, endpoint, path, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", endpoint)
	if csrf != "" {
		req.Header.Set("X-Relay-CSRF", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// webCSRF resolves the live session's CSRF token for a cookie client.
func webCSRF(t *testing.T, d *Daemon, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(d.endpoint() + "/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.CSRFToken == "" {
		t.Fatalf("auth/session csrf: %v", err)
	}
	return s.CSRFToken
}

func decodeSessionResp(t *testing.T, resp *http.Response) api.SessionResponse {
	t.Helper()
	defer resp.Body.Close()
	var out api.SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return out
}

// TestWebCookieSessionControl: the full browser lifecycle on one session —
// create COLD, detail, prompt (canonical user+agent records), config
// (session.config), durable delete, post-delete 404.
func TestWebCookieSessionControl(t *testing.T) {
	d, p, c := webDaemonCodex(t, "happy")
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	// Create through the cookie domain — runtime-free COLD session.
	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, `{"key":"webctl","cwd":"/","model":"gpt-5-codex"}`)
	created := decodeSessionResp(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if s := created.Session; s.RuntimeState != session.RuntimeCold || s.PID != 0 || s.Generation != 0 {
		t.Fatalf("create must be COLD/runtime-free: %+v", s)
	}
	// Detail read — also a pure observer.
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/webctl", "", "")
	got := decodeSessionResp(t, resp)
	if got.Session.Key != "webctl" || got.Session.RuntimeState != session.RuntimeCold {
		t.Fatalf("detail: %+v", got.Session)
	}

	// Prompt through the cookie domain; the canonical records arrive.
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/webctl/prompt",
		csrf, `{"text":"hello from the web"}`)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("prompt: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	cli := daemonClient(t, p)
	page := waitDurableTypes(t, cli, "webctl", api.EventMessageAgentCompleted)
	var user, agent bool
	for _, r := range page.Records {
		switch r.Type {
		case api.EventMessageUser:
			user = true
		case api.EventMessageAgentCompleted:
			agent = true
		}
	}
	if !user || !agent {
		t.Fatal("canonical message.user / message.agent.completed missing")
	}

	// Config through the cookie domain → session.config is durable.
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/sessions/webctl/config",
		csrf, `{"model":"gpt-5.1-codex"}`)
	got = decodeSessionResp(t, resp)
	if resp.StatusCode != http.StatusOK || got.Session.Model != "gpt-5.1-codex" {
		t.Fatalf("config: %d %+v", resp.StatusCode, got.Session)
	}
	waitDurableTypes(t, cli, "webctl", api.EventConfigChanged)

	// Delete through the cookie domain → durable removal; detail 404s.
	resp = cookieJSON(t, c, http.MethodDelete, endpoint, "/v1/sessions/webctl", csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/webctl", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d", resp.StatusCode)
	}
}

// TestWebCookieCancelAndInput: cancel reconciles a waiting turn to
// turn.interrupted, and a requested-input answer resolves durably.
func TestWebCookieCancelAndInput(t *testing.T) {
	d, p, c := webDaemonCodex(t, "input")
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	cli := daemonClient(t, p)

	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, `{"key":"cancelme","cwd":"/"}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/cancelme/prompt",
		csrf, `{"text":"hold"}`)
	resp.Body.Close()
	waitDurableTypes(t, cli, "cancelme", api.EventInputRequested)

	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/cancelme/cancel",
		csrf, "{}")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	waitDurableTypes(t, cli, "cancelme", api.EventTurnInterrupted)
	// The aborted input is recorded too — pending state reconciles.
	waitDurableTypes(t, cli, "cancelme", api.EventInputAborted)
	// A second cancel is a clean NO_ACTIVE_TURN, not a 500.
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/cancelme/cancel",
		csrf, "{}")
	defer resp.Body.Close()
	var env api.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict || env.Error.Code != api.ErrNoActiveTurn {
		t.Fatalf("second cancel: %d %v", resp.StatusCode, env.Error)
	}
}

// TestWebCookieInputAnswer: a requested-input question is answered through
// the cookie domain and input.resolved lands durably.
func TestWebCookieInputAnswer(t *testing.T) {
	d, p, c := webDaemonCodex(t, "input")
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	cli := daemonClient(t, p)

	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, `{"key":"answerme","cwd":"/"}`)
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/answerme/prompt",
		csrf, `{"text":"ask me"}`)
	resp.Body.Close()
	page := waitDurableTypes(t, cli, "answerme", api.EventInputRequested)
	var req struct {
		InputID string `json:"inputId"`
	}
	for _, r := range page.Records {
		if r.Type == api.EventInputRequested {
			if err := json.Unmarshal(r.Payload, &req); err != nil {
				t.Fatal(err)
			}
		}
	}
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/answerme/input",
		csrf, fmt.Sprintf(`{"inputId":%q,"answers":[{"questionId":"q1","answers":["alpha"]}]}`, req.InputID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("input answer: %d", resp.StatusCode)
	}
	waitDurableTypes(t, cli, "answerme", api.EventInputResolved)
	waitDurableTypes(t, cli, "answerme", api.EventMessageAgentCompleted)
}
