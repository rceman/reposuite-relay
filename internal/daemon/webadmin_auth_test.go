package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
)

func cookiejarForTest() (*cookiejar.Jar, error) { return cookiejar.New(nil) }

// cookieRequest builds a /v1 request carrying the admin cookie.
func cookieRequest(t *testing.T, method, endpoint, path, token, csrf, origin string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, endpoint+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{
		Name:  adminCookieName,
		Value: token,
	})
	if csrf != "" {
		req.Header.Set("X-Relay-CSRF", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func doReq(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCookieAuthOnV1(t *testing.T) {
	d, c := webDaemon(t)
	endpoint := d.endpoint()
	token := setupAdmin(t, d, c, "admin", "correct-horse-12")
	sess, _, ok := d.web.cookieSession(mustCookieReq(t, endpoint, token))
	if !ok {
		t.Fatal("session not live")
	}

	// GET with the cookie works.
	resp := doReq(t, cookieRequest(t, http.MethodGet, endpoint, "/v1/daemon", token, "", ""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie GET /v1/daemon: %d", resp.StatusCode)
	}
	// Unsafe without CSRF → 403.
	resp = doReq(t, cookieRequest(t, http.MethodPost, endpoint, "/v1/sessions/fixture", token, "", endpoint))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF: %d", resp.StatusCode)
	}
	// Unsafe with wrong Origin → 403.
	resp = doReq(t, cookieRequest(t, http.MethodPost, endpoint, "/v1/sessions/fixture",
		token, sess.CSRFToken, "http://evil.example"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST wrong Origin: %d", resp.StatusCode)
	}
	// Unsafe with Origin + valid CSRF → admitted (fixture create may
	// succeed or fail downstream; what matters is passing auth).
	resp = doReq(t, cookieRequest(t, http.MethodPost, endpoint, "/v1/sessions/fixture",
		token, sess.CSRFToken, endpoint))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("cookie POST with CSRF rejected: %d %q", resp.StatusCode, body)
	}
	// Bearer auth needs no CSRF — regression for machine clients.
	resp = doReq(t, func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, endpoint+"/v1/daemon", nil)
		req.Header.Set("Authorization", "Bearer "+d.machineToken)
		return req
	}())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("machine bearer GET: %d", resp.StatusCode)
	}
}

func TestHostHeaderEnforcement(t *testing.T) {
	d, _ := webDaemon(t)
	endpoint := d.endpoint()
	_, port, _ := strings.Cut(strings.TrimPrefix(endpoint, "http://"), ":")

	for name, host := range map[string]string{
		"attacker host": "evil.example:" + port,
		"localhost":     "localhost:" + port,
		"wrong port":    "127.0.0.1:1",
		"ipv6 loopback": "[::1]:" + port,
		"no port":       "127.0.0.1",
	} {
		req, err := http.NewRequest(http.MethodGet, endpoint+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp := doReq(t, req)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s (%q) accepted", name, host)
		}
	}
	// Canonical host accepted.
	resp := rawGet(t, endpoint, "/", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("canonical host rejected: %d", resp.StatusCode)
	}
	// Bearer /v1 with canonical host still fine.
	req, _ := http.NewRequest(http.MethodGet, endpoint+"/v1/daemon", nil)
	req.Header.Set("Authorization", "Bearer "+d.machineToken)
	resp = doReq(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("canonical host /v1: %d", resp.StatusCode)
	}
}

func TestAuthSessionEndpoint(t *testing.T) {
	d, c := webDaemon(t)
	endpoint := d.endpoint()

	get := func(token string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, endpoint+"/auth/session", nil)
		if token != "" {
			req.AddCookie(&http.Cookie{
				Name:  adminCookieName,
				Value: token,
			})
		}
		resp := doReq(t, req)
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Unconfigured.
	out := get("")
	if out["configured"] != false || out["authenticated"] != false {
		t.Fatalf("unconfigured: %v", out)
	}
	token := setupAdmin(t, d, c, "admin", "correct-horse-12")
	out = get("")
	if out["configured"] != true || out["authenticated"] != false {
		t.Fatalf("configured-unauthenticated: %v", out)
	}
	out = get(token)
	if out["authenticated"] != true || out["username"] != "admin" {
		t.Fatalf("authenticated: %v", out)
	}
	if out["csrfToken"] == "" {
		t.Fatal("authenticated response lacks csrfToken")
	}
	// The session token itself is never returned.
	if strings.Contains(fmt.Sprint(out), token) {
		t.Fatal("browser session token leaked in /auth/session")
	}
}

func TestBrowserSessionDiesWithDaemon(t *testing.T) {
	p := testPaths(t)
	d1, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejarForTest()
	c1 := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	done1 := serveDaemon(t, d1)
	token := setupAdmin(t, d1, c1, "admin", "correct-horse-12")
	stopDaemonNow(t, d1, done1)

	d2, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	// Same endpoint (stable port), same cookie value, dead session.
	req, _ := http.NewRequest(http.MethodGet, d2.endpoint()+"/auth/session", nil)
	req.AddCookie(&http.Cookie{
		Name:  adminCookieName,
		Value: token,
	})
	resp := doReq(t, req)
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["authenticated"] != false {
		t.Fatalf("stale cookie authenticated after restart: %v", out)
	}
}

// TestSetupDoesNotLeakSecrets proves no credential material crosses the
// wire: the password, hash, and machine token are absent from responses.
func TestSetupDoesNotLeakSecrets(t *testing.T) {
	d, c := webDaemon(t)
	endpoint := d.endpoint()
	token := setupAdmin(t, d, c, "admin", "correct-horse-12")

	for _, path := range []string{"/", "/auth/session"} {
		req, _ := http.NewRequest(http.MethodGet, endpoint+path, nil)
		req.AddCookie(&http.Cookie{
			Name:  adminCookieName,
			Value: token,
		})
		resp := doReq(t, req)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, secret := range []string{"correct-horse-12", d.machineToken, token} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("%s leaked secret material", path)
			}
		}
	}
	// admin.json never contains the plaintext.
	raw, _ := os.ReadFile(d.paths.AdminCredentials())
	if strings.Contains(string(raw), "correct-horse-12") {
		t.Fatal("admin.json contains the plaintext password")
	}
	if !strings.Contains(string(raw), "$argon2id$") {
		t.Fatal("admin.json lacks a PHC record")
	}
}
