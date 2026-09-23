// Machine API token management over /v1/settings/machine-token*: the
// admin-cookie-only credential surface — authorization domains, live
// rotation authority, commit-point classification, restart persistence,
// concurrent serialization, and non-disclosure of the secret.
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/auth"
)

// cookieJSONDo is cookieJSON without t.Fatal — safe for worker
// goroutines; the caller asserts on the returned values.
func cookieJSONDo(c *http.Client, method, endpoint, path, csrf, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, endpoint+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", endpoint)
	if csrf != "" {
		req.Header.Set("X-Relay-CSRF", csrf)
	}
	return c.Do(req)
}

// bearerGetStatus is bearerStatus without t.Fatal — for goroutines.
func bearerGetStatus(endpoint, bearer string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint+"/v1/daemon", nil)
	if err != nil {
		return 0, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// rawPost is a bearer-authed POST — machine/descriptor credential domain.
func rawPost(t *testing.T, endpoint, path, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeRotate(t *testing.T, resp *http.Response) api.MachineTokenRotateResponse {
	t.Helper()
	defer resp.Body.Close()
	var out api.MachineTokenRotateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("rotate response: %v", err)
	}
	return out
}

// bearerStatus performs a harmless read with a bearer credential and
// returns the HTTP status — the admission verdict for that credential.
func bearerStatus(t *testing.T, endpoint, bearer string) int {
	t.Helper()
	resp := rawGet(t, endpoint, "/v1/daemon", bearer)
	resp.Body.Close()
	return resp.StatusCode
}

// TestMachineTokenAdminOnly: token management rejects every credential
// domain except the authenticated admin browser session.
func TestMachineTokenAdminOnly(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	machine, err := auth.Load(d.paths.MachineToken())
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/v1/settings/machine-token",
		"/v1/settings/machine-token/rotate",
	} {
		// Anonymous: 401.
		resp := rawGet(t, endpoint, path, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", path, resp.StatusCode)
		}
		// Descriptor bearer: 403 ADMIN_REQUIRED.
		resp = rawGet(t, endpoint, path, d.Token())
		env := decodeErrBody(t, resp)
		if resp.StatusCode != http.StatusForbidden || env.Error.Code != api.ErrAdminRequired {
			t.Fatalf("descriptor %s: %d %q", path, resp.StatusCode, env.Error.Code)
		}
		// Machine bearer: 403 — the credential cannot manage itself.
		resp = rawGet(t, endpoint, path, machine)
		env = decodeErrBody(t, resp)
		if resp.StatusCode != http.StatusForbidden || env.Error.Code != api.ErrAdminRequired {
			t.Fatalf("machine %s: %d %q", path, resp.StatusCode, env.Error.Code)
		}
	}
	// Admin cookie GET: 200, configured, no token field in the body.
	resp := cookieJSON(t, c, http.MethodGet, endpoint, "/v1/settings/machine-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin status: %d", resp.StatusCode)
	}
	var status map[string]any
	json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if status["configured"] != true {
		t.Fatalf("status = %+v", status)
	}
	if _, leaked := status["token"]; leaked {
		t.Fatal("status response leaked a token field")
	}
	// Admin POST without CSRF → 403; wrong Origin → 403.
	resp = cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rotate without CSRF: %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodPost,
		endpoint+"/v1/settings/machine-token/rotate", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("X-Relay-CSRF", csrf)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rotate with foreign Origin: %d", resp.StatusCode)
	}
}

// TestMachineTokenLiveRotation: the core live-authority proof — after
// rotation the new machine token authenticates, the old one is dead for
// new requests, and descriptor + admin domains are untouched.
func TestMachineTokenLiveRotation(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, err := auth.Load(d.paths.MachineToken())
	if err != nil {
		t.Fatal(err)
	}
	if s := bearerStatus(t, endpoint, old); s != http.StatusOK {
		t.Fatalf("old token before rotation: %d", s)
	}
	resp := cookieJSON(t, c, http.MethodPost, endpoint,
		"/v1/settings/machine-token/rotate", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("rotate response Cache-Control = %q", cc)
	}
	out := decodeRotate(t, resp)
	if !auth.Valid(out.Token) || out.Token == old {
		t.Fatal("rotate did not return a fresh valid token")
	}
	if !out.DurabilityConfirmed {
		t.Fatal("clean rotation must confirm durability")
	}
	// New credential domain verdicts — no restart.
	if s := bearerStatus(t, endpoint, out.Token); s != http.StatusOK {
		t.Fatalf("new token: %d", s)
	}
	if s := bearerStatus(t, endpoint, old); s != http.StatusUnauthorized {
		t.Fatalf("old token after rotation: %d", s)
	}
	if s := bearerStatus(t, endpoint, d.Token()); s != http.StatusOK {
		t.Fatalf("descriptor bearer: %d", s)
	}
	// The rotating admin session is still authenticated.
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/settings/machine-token", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin session after rotation: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Canonical file carries the committed credential.
	got, err := auth.Load(d.paths.MachineToken())
	if err != nil || got != out.Token {
		t.Fatal("disk/live credential divergence")
	}
}

// TestMachineAuthLinearization — deterministic auth-vs-rotation proof:
// a compare that ENTERS the RLock section before rotation linearizes
// with the old credential (and rotation cannot complete until it
// releases); every compare after the switch sees only the committed
// token. No sleeps — the gate channel is the synchronization point.
func TestMachineAuthLinearization(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		MachineAuthGate: func() {
			// Park the first compare inside the read lock.
			once.Do(func() { close(entered); <-release })
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)
	old, err := auth.Load(d.paths.MachineToken())
	if err != nil {
		t.Fatal(err)
	}

	// The OLD-token request parks inside the RLock compare.
	authDone := make(chan int, 1)
	go func() {
		code, err := bearerGetStatus(endpoint, old)
		if err != nil {
			authDone <- -1
			return
		}
		authDone <- code
	}()
	<-entered // compare is inside the read lock

	// Start a rotation — it must queue on the write lock and cannot
	// commit while the reader holds RLock. TryLock proves the lock is
	// unavailable: a committed rotation would have released it.
	rotDone := make(chan api.MachineTokenRotateResponse, 1)
	go func() {
		resp, err := cookieJSONDo(c, http.MethodPost, endpoint,
			"/v1/settings/machine-token/rotate", csrf, "")
		if err != nil || resp.StatusCode != http.StatusOK {
			rotDone <- api.MachineTokenRotateResponse{}
			return
		}
		defer resp.Body.Close()
		var out api.MachineTokenRotateResponse
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			rotDone <- api.MachineTokenRotateResponse{}
			return
		}
		rotDone <- out
	}()
	// A free lock would mean the rotation already committed — it can't.
	if d.machineMu.TryLock() {
		d.machineMu.Unlock()
		t.Fatal("rotation committed while a reader owned the compare")
	}
	close(release) // the parked compare now linearizes BEFORE rotation
	if code := <-authDone; code != http.StatusOK {
		t.Fatalf("pre-rotation OLD-token compare = %d, want 200", code)
	}
	out := <-rotDone
	if !auth.Valid(out.Token) || out.Token == old {
		t.Fatal("rotation did not commit a fresh token")
	}
	// Post-commit: only the new credential authenticates.
	if s := bearerStatus(t, endpoint, old); s != http.StatusUnauthorized {
		t.Fatalf("old token post-rotation: %d", s)
	}
	if s := bearerStatus(t, endpoint, out.Token); s != http.StatusOK {
		t.Fatalf("new token post-rotation: %d", s)
	}
}
