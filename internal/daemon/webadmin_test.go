package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
)

// webDaemon starts a daemon with the cheap test KDF and returns it plus a
// browser-like client (cookie jar, no redirects).
func webDaemon(t *testing.T) (*Daemon, *http.Client) {
	t.Helper()
	d, err := Start(testPaths(t), Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// mustCookieReq builds a request carrying the raw admin cookie value.
func mustCookieReq(t *testing.T, endpoint, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{
		Name:  adminCookieName,
		Value: token,
	})
	return req
}

// setupAdmin performs a full valid setup through HTTP and returns the
// session cookie value.
func setupAdmin(t *testing.T, d *Daemon, c *http.Client, username, password string) string {
	t.Helper()
	resp, err := c.Do(webForm(d, "setup", url.Values{
		"form_token": {d.web.mgr.FormToken("setup")},
		"username":   {username},
		"password":   {password},
		"confirm":    {password},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	cookies := c.Jar.Cookies(mustURL(t, d.endpoint()))
	for _, ck := range cookies {
		if ck.Name == adminCookieName {
			return ck.Value
		}
	}
	t.Fatal("setup did not issue a session cookie")
	return ""
}

// getSessionState GETs /auth/session and decodes it into out — the SPA
// bootstrap endpoint carrying configured/authenticated/formToken/CSRF.
func getSessionState(t *testing.T, c *http.Client, endpoint string, out any) {
	t.Helper()
	resp, err := c.Get(endpoint + "/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/auth/session: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("/auth/session decode: %v", err)
	}
}

func webForm(d *Daemon, action string, fields url.Values) *http.Request {
	req, err := http.NewRequest(http.MethodPost,
		d.endpoint()+"/auth/"+action, strings.NewReader(fields.Encode()))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", d.endpoint())
	return req
}

func TestSetupLoginLogoutFlow(t *testing.T) {
	d, c := webDaemon(t)
	endpoint := d.endpoint()

	// State A: no admin — root serves the anonymous SPA shell in every
	// auth state; /auth/session carries the setup-mode projection.
	resp, err := c.Get(endpoint + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, d.static.index) {
		t.Fatalf("root is not the embedded SPA entry: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type %q", ct)
	}
	// Security headers present on web responses.
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options",
		"Referrer-Policy", "X-Frame-Options", "Cache-Control"} {
		if resp.Header.Get(h) == "" {
			t.Fatalf("missing security header %s", h)
		}
	}
	var sess struct {
		Configured    bool   `json:"configured"`
		Authenticated bool   `json:"authenticated"`
		FormToken     string `json:"formToken"`
	}
	getSessionState(t, c, endpoint, &sess)
	if sess.Configured || sess.Authenticated || sess.FormToken == "" {
		t.Fatalf("bootstrap state wrong: %+v", sess)
	}
	if sess.FormToken != d.web.mgr.FormToken("setup") {
		t.Fatal("bootstrap formToken is not the setup token")
	}

	setupToken := d.web.mgr.FormToken("setup")
	do := func(fields url.Values) *http.Response {
		resp, err := c.Do(webForm(d, "setup", fields))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Bad inputs rejected.
	for name, mutate := range map[string]func(url.Values){
		"bad username":     func(v url.Values) { v.Set("username", "bad user") },
		"short password":   func(v url.Values) { v.Set("password", "short"); v.Set("confirm", "short") },
		"confirm mismatch": func(v url.Values) { v.Set("confirm", "different-pass!!") },
		"bad form token":   func(v url.Values) { v.Set("form_token", "deadbeef") },
	} {
		v := url.Values{"form_token": {setupToken}, "username": {"admin"},
			"password": {"correct-horse-12"}, "confirm": {"correct-horse-12"}}
		mutate(v)
		resp := do(v)
		resp.Body.Close()
		if resp.StatusCode == http.StatusSeeOther {
			t.Fatalf("%s: setup succeeded", name)
		}
	}
	// Wrong Origin rejected.
	v := url.Values{"form_token": {setupToken}, "username": {"admin"},
		"password": {"correct-horse-12"}, "confirm": {"correct-horse-12"}}
	req := webForm(d, "setup", v)
	req.Header.Set("Origin", "http://evil.example")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin setup: %d", resp.StatusCode)
	}

	// Valid setup commits and authenticates.
	resp = do(v)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid setup: %d %q", resp.StatusCode, raw)
	}
	if _, err := adminauth.Load(d.paths.AdminCredentials()); err != nil {
		t.Fatalf("admin.json not committed: %v", err)
	}
	if fi, _ := os.Stat(d.paths.AdminCredentials()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("admin.json mode %o", fi.Mode().Perm())
	}
	// The session cookie is HttpOnly + SameSite=Strict + Path=/ (and no
	// Secure flag: the canonical transport is loopback HTTP).
	var setCookie string
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, adminCookieName+"=") {
			setCookie = h
		}
	}
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(setCookie, want) {
			t.Fatalf("Set-Cookie %q missing %q", setCookie, want)
		}
	}
	if strings.Contains(setCookie, "Secure") {
		t.Fatalf("loopback cookie must not set Secure: %q", setCookie)
	}
	// Authenticated: /auth/session reports the username + CSRF token and
	// no form token; the SPA document itself stays the anonymous shell.
	var authSess struct {
		Configured    bool   `json:"configured"`
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
		CSRFToken     string `json:"csrfToken"`
		FormToken     string `json:"formToken"`
	}
	getSessionState(t, c, endpoint, &authSess)
	if !authSess.Configured || !authSess.Authenticated ||
		authSess.Username != "admin" || authSess.CSRFToken == "" ||
		authSess.FormToken != "" {
		t.Fatalf("authenticated bootstrap wrong: %+v", authSess)
	}
	// Second setup is rejected.
	resp = do(v)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second setup: %d", resp.StatusCode)
	}

	// Logout requires the session CSRF token.
	resp, err = c.Do(webForm(d, "logout", url.Values{}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", resp.StatusCode)
	}
	s, _, ok := d.web.cookieSession(mustCookieReq(t, endpoint,
		c.Jar.Cookies(mustURL(t, endpoint))[0].Value))
	if !ok {
		t.Fatal("session lost")
	}
	resp, err = c.Do(webForm(d, "logout", url.Values{adminCSRFField: {s.CSRFToken}}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	// Cookie revoked: /auth/session is back to the login projection.
	var loggedOut struct {
		Configured    bool   `json:"configured"`
		Authenticated bool   `json:"authenticated"`
		FormToken     string `json:"formToken"`
	}
	getSessionState(t, c, endpoint, &loggedOut)
	if !loggedOut.Configured || loggedOut.Authenticated ||
		loggedOut.FormToken == "" {
		t.Fatalf("post-logout bootstrap wrong: %+v", loggedOut)
	}
	if loggedOut.FormToken != d.web.mgr.FormToken("login") {
		t.Fatal("post-logout formToken is not the login token")
	}
	// Double logout fails deterministically.
	resp, err = c.Do(webForm(d, "logout", url.Values{adminCSRFField: {s.CSRFToken}}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("double logout: %d", resp.StatusCode)
	}

	// Login: wrong password and wrong username share the failure shape.
	loginToken := d.web.mgr.FormToken("login")
	for name, v := range map[string]url.Values{
		"wrong password": {"form_token": {loginToken}, "username": {"admin"}, "password": {"wrong-password!"}},
		"wrong username": {"form_token": {loginToken}, "username": {"nobody"}, "password": {"correct-horse-12"}},
	} {
		resp, err := c.Do(webForm(d, "login", v))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", name, resp.StatusCode)
		}
	}
	// Correct credentials authenticate.
	resp, err = c.Do(webForm(d, "login", url.Values{"form_token": {loginToken},
		"username": {"admin"}, "password": {"correct-horse-12"}}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}
