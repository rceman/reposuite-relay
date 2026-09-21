package daemon

import (
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
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
