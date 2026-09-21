package daemon

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// fakeClock is the test time source for browser auth.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// webDaemonClock starts a daemon with a fake browser-auth clock.
func webDaemonClock(t *testing.T, clock *fakeClock) *Daemon {
	t.Helper()
	d, err := Start(testPaths(t), Options{
		SelfExe:    testBinary(),
		AdminKDF:   adminauth.TestParams,
		AdminClock: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return d
}

func TestLoginThrottlingHTTP(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := webDaemonClock(t, clock)
	c, _ := cookiejarForTest()
	client := &http.Client{
		Jar: c,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	setupAdmin(t, d, client, "admin", "correct-horse-12")
	loginToken := d.web.mgr.FormToken("login")

	failLogin := func() *http.Response {
		resp, err := client.Do(webForm(d, "login", url.Values{
			"form_token": {loginToken}, "username": {"admin"}, "password": {"wrong-password!"}}))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for i := 0; i < adminauth.LoginMaxFailures; i++ {
		if resp := failLogin(); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := failLogin()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("throttled login: %d, want 429", resp.StatusCode)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > int(adminauth.LoginThrottleFor/time.Second)+1 {
		t.Fatalf("Retry-After %q out of bounds", resp.Header.Get("Retry-After"))
	}
	clock.Advance(adminauth.LoginThrottleFor + time.Second)
	resp, err = client.Do(webForm(d, "login", url.Values{
		"form_token": {loginToken}, "username": {"admin"}, "password": {"correct-horse-12"}}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login after throttle window: %d", resp.StatusCode)
	}
}

func TestMalformedAdminCredentialFailsStartup(t *testing.T) {
	for name, mutate := range map[string]func(string){
		"malformed":  func(p string) { writeFileMode(t, p, "{bad", 0o600) },
		"insecure":   func(p string) { writeFileMode(t, p, validAdminJSON(t), 0o644) },
		"bad schema": func(p string) { writeFileMode(t, p, `{"schemaVersion":9,"username":"a","passwordHash":"x"}`, 0o600) },
		"bad PHC": func(p string) {
			writeFileMode(t, p, `{"schemaVersion":1,"username":"admin","passwordHash":"x"}`, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			if err := p.Ensure(); err != nil {
				t.Fatal(err)
			}
			mutate(p.AdminCredentials())
			if _, err := Start(p, Options{
				SelfExe:  testBinary(),
				AdminKDF: adminauth.TestParams,
			}); err == nil {
				t.Fatal("startup accepted invalid admin.json")
			}
			if _, err := pathsHasDescriptor(p); err == nil {
				t.Fatal("failed startup left a descriptor")
			}
		})
	}
}

func validAdminJSON(t *testing.T) string {
	t.Helper()
	hash, err := adminauth.Hash("a-valid-test-password", adminauth.TestParams)
	if err != nil {
		t.Fatal(err)
	}
	return `{"schemaVersion":1,"username":"admin","passwordHash":"` + hash + `"}`
}

func writeFileMode(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func pathsHasDescriptor(p paths.Paths) (bool, error) {
	_, err := os.Stat(p.DaemonDescriptor())
	if err == nil {
		return true, nil
	}
	return false, err
}

// TestSetupPostCommitFailsClosed proves the commit-point contract at the
// HTTP layer: a directory-fsync failure AFTER the canonical link must
// not roll anything back, must not leave the daemon in setup mode, must
// not issue a session, and must initiate shutdown so restart reconciles
// the durable credential.
func TestSetupPostCommitFailsClosed(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		AdminHooks: &adminauth.Hooks{
			SyncDir: func(string) error { return errors.New("injected dirsync failure") },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := serveDaemon(t, d)
	jar, _ := cookiejarForTest()
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := c.Do(webForm(d, "setup", url.Values{
		"form_token": {d.web.mgr.FormToken("setup")},
		"username":   {"admin"},
		"password":   {"correct-horse-12"},
		"confirm":    {"correct-horse-12"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("post-commit setup: %d, want 500", resp.StatusCode)
	}
	// No session cookie was issued.
	if len(jar.Cookies(mustURL(t, d.endpoint()))) != 0 {
		t.Fatal("post-commit uncertainty issued a session cookie")
	}
	// The committed credential IS canonical — readable from disk, and the
	// daemon recognized it in-memory: setup mode can never run again in
	// this generation even while shutdown is already draining.
	loaded, err := adminauth.Load(p.AdminCredentials())
	if err != nil || loaded.Username != "admin" {
		t.Fatalf("committed credential unreadable: %v", err)
	}
	if !d.web.configured() {
		t.Fatal("daemon still considers itself unconfigured after committed credential")
	}
	// Shutdown was initiated — the operator restart reconciles state.
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("daemon did not shut down after post-commit uncertainty")
	}
	// After the restart, the durable credential loads into login mode —
	// setup never runs again.
	d2, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	resp, err = c.Get(d2.endpoint() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Web Admin setup") {
		t.Fatal("restart still serving setup mode with a committed credential")
	}
	if !strings.Contains(string(body), "Web Admin sign in") {
		t.Fatalf("expected login page after restart: %q", body)
	}
}

// TestLoginSingleVerification proves every admitted login attempt runs
// exactly one password verification against the stored credential —
// wrong username included (no dummy-hash double-hash path).
func TestLoginSingleVerification(t *testing.T) {
	p := testPaths(t)
	var calls atomic.Int32
	var lastEncoded atomic.Value
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		AdminVerify: func(pw, enc string) bool {
			calls.Add(1)
			lastEncoded.Store(enc)
			return adminauth.Verify(pw, enc)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejarForTest()
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	serveDaemon(t, d)
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	stored, err := adminauth.Load(p.AdminCredentials())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("setup must not perform password verification")
	}
	loginToken := d.web.mgr.FormToken("login")
	attempt := func(user, pass string, want int) {
		before := calls.Load()
		resp, err := c.Do(webForm(d, "login", url.Values{
			"form_token": {loginToken}, "username": {user}, "password": {pass}}))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("login(%q): %d, want %d", user, resp.StatusCode, want)
		}
		if got := calls.Load() - before; got != 1 {
			t.Fatalf("login(%q) ran %d verifications, want exactly 1", user, got)
		}
		if lastEncoded.Load() != stored.PasswordHash {
			t.Fatalf("login(%q) verified against a non-stored hash", user)
		}
	}
	attempt("nobody", "wrong-password!", http.StatusUnauthorized)
	attempt("nobody", "correct-horse-12", http.StatusUnauthorized)
	attempt("admin", "wrong-password!", http.StatusUnauthorized)
	attempt("admin", "correct-horse-12", http.StatusSeeOther)
}
