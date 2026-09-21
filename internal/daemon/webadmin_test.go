package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
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

	// State A: no admin → setup page.
	resp, err := c.Get(endpoint + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Web Admin setup") {
		t.Fatalf("root is not the setup page: %q", body)
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
	// Authenticated root renders the shell.
	resp, err = c.Get(endpoint + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Signed in as admin") {
		t.Fatalf("authenticated shell missing: %q", body)
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
	// Cookie revoked: root is the login page again.
	resp, err = c.Get(endpoint + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Web Admin sign in") {
		t.Fatalf("post-logout root is not the login page: %q", body)
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

func TestConcurrentSetupCommitsOnce(t *testing.T) {
	d, _ := webDaemon(t)
	setupToken := d.web.mgr.FormToken("setup")
	const n = 8
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := webForm(d, "setup", url.Values{"form_token": {setupToken},
				"username": {fmt.Sprintf("admin%d", i)},
				"password": {"correct-horse-12"}, "confirm": {"correct-horse-12"}})
			resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}).Do(req)
			if err != nil {
				results <- -1
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}(i)
	}
	wg.Wait()
	close(results)
	wins := 0
	for code := range results {
		if code == http.StatusSeeOther {
			wins++
		} else if code != http.StatusConflict {
			t.Fatalf("unexpected setup result %d", code)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent setups committed, want exactly 1", wins)
	}
	if _, err := adminauth.Load(d.paths.AdminCredentials()); err != nil {
		t.Fatalf("credential not committed: %v", err)
	}
}
