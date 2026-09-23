package daemon

// GET /v1/runtimes — global live-runtime inventory. Observer-only:
// no COLD wake, no durable writes, no secrets.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
)

// doJSON performs one authenticated request and returns status+body.
func doJSON(t *testing.T, endpoint, bearer, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// readSessionMeta returns the durable session.json bytes — byte-identical
// comparison proves the observer wrote nothing.
func readSessionMeta(t *testing.T, sessionsDir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionsDir, id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func getRuntimes(t *testing.T, endpoint, bearer string) (*http.Response, api.RuntimeList) {
	t.Helper()
	resp := rawGet(t, endpoint, "/v1/runtimes", bearer)
	return resp, decodeRuntimes(t, resp)
}

func decodeRuntimes(t *testing.T, resp *http.Response) api.RuntimeList {
	t.Helper()
	var out api.RuntimeList
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	resp.Body.Close()
	return out
}

func TestRuntimesAuthAndEmpty(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := desc.Endpoint

	// Anonymous → 401.
	resp, _ := getRuntimes(t, endpoint, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", resp.StatusCode)
	}
	// POST → 405 (GET-only observer route).
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/v1/runtimes", nil)
	req.Header.Set("Authorization", "Bearer "+desc.BearerToken)
	r2, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", r2.StatusCode)
	}
	// Descriptor bearer + machine token both authenticate.
	for _, bearer := range []string{desc.BearerToken, d.Token()} {
		resp, out := getRuntimes(t, endpoint, bearer)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("authed = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if len(out.Runtimes) != 0 || out.Totals.RuntimeCount != 0 {
			t.Fatalf("expected empty inventory: %+v", out.Totals)
		}
		if out.SampledAt == "" {
			t.Fatal("sampledAt missing")
		}
	}
}

// TestRuntimesNeverWakesCold: reading /v1/runtimes must not wake a COLD
// session, spawn anything, or touch durable state.
func TestRuntimesNeverWakesCold(t *testing.T) {
	p := testPaths(t)
	writeColdSession(t, p.SessionsDir(), "coldone", "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", 5)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	desc, _ := client.ReadDescriptor(p)

	before := readSessionMeta(t, p.SessionsDir(), "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6")
	for i := 0; i < 5; i++ {
		resp, out := getRuntimes(t, desc.Endpoint, desc.BearerToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if out.Totals.RuntimeCount != 0 {
			t.Fatalf("runtime appeared: %+v", out.Totals)
		}
	}
	// COLD session untouched: generation identical, still no runtime.
	after := readSessionMeta(t, p.SessionsDir(), "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6")
	if before != after {
		t.Fatal("durable session.json mutated by observer read")
	}
	m, ok := d.registry.Get("coldone")
	if !ok {
		t.Fatal("session missing from registry")
	}
	if s := m.Snapshot(); s.Generation != 1 {
		t.Fatalf("generation changed to %d", s.Generation)
	}
}

// TestRuntimesLiveFixture: one fixture runtime is inventoried with PID,
// uptime, binding, and a real process-tree sample on Linux.
func TestRuntimesLiveFixture(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	desc, _ := client.ReadDescriptor(p)

	status, out := doJSON(t, desc.Endpoint, desc.BearerToken, http.MethodPost, "/v1/sessions/fixture",
		`{"key":"obs-fix","cwd":"/"}`)
	if status != http.StatusOK {
		t.Fatalf("fixture create = %d %s", status, out)
	}

	resp, list := getRuntimes(t, desc.Endpoint, desc.BearerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if list.Totals.RuntimeCount != 1 || len(list.Runtimes) != 1 {
		t.Fatalf("inventory: %+v", list.Totals)
	}
	rt := list.Runtimes[0]
	if rt.Harness != "fixture" || rt.PID <= 0 || rt.RuntimeID == "" {
		t.Fatalf("runtime: %+v", rt)
	}
	if rt.UptimeSeconds < 0 {
		t.Fatalf("uptime %d", rt.UptimeSeconds)
	}
	if rt.SessionCount != 1 || len(rt.Sessions) != 1 ||
		rt.Sessions[0].Key != "obs-fix" || rt.Sessions[0].Activity != "idle" {
		t.Fatalf("bindings: %+v", rt.Sessions)
	}
	if !rt.Resources.Available {
		t.Skip("resource sampling unavailable on this platform")
	}
	if rt.Resources.PSSBytes <= 0 || rt.Resources.RSSBytes <= 0 || rt.Resources.ProcessCount < 1 {
		t.Fatalf("resources: %+v", rt.Resources)
	}
	if list.Totals.MeasuredRuntimeCount != 1 ||
		list.Totals.PSSBytes != rt.Resources.PSSBytes {
		t.Fatalf("totals: %+v", list.Totals)
	}

	// Response must never contain credentials or native IDs.
	raw := string(api.Marshal(list))
	for _, banned := range []string{desc.BearerToken, d.Token(), "nativeSessionId"} {
		if banned != "" && strings.Contains(raw, banned) {
			t.Fatalf("response leaks %q", banned)
		}
	}

	// Deleting the fixture session returns the inventory to empty.
	status, out = doJSON(t, desc.Endpoint, desc.BearerToken, http.MethodDelete, "/v1/sessions/obs-fix", "")
	if status != http.StatusOK {
		t.Fatalf("delete = %d %s", status, out)
	}
	_, list = getRuntimes(t, desc.Endpoint, desc.BearerToken)
	if list.Totals.RuntimeCount != 0 {
		t.Fatalf("runtime leaked after delete: %+v", list.Totals)
	}
}

// TestRuntimesResponseIsDeterministic: two reads of a stable inventory
// serialize identically except daemon uptime/sampledAt.
func TestRuntimesSecretFree(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	desc, _ := client.ReadDescriptor(p)
	if status, out := doJSON(t, desc.Endpoint, desc.BearerToken, http.MethodPost,
		"/v1/sessions/fixture", `{"key":"x","cwd":"/"}`); status != http.StatusOK {
		t.Fatalf("fixture create = %d %s", status, out)
	}
	resp := rawGet(t, desc.Endpoint, "/v1/runtimes", desc.BearerToken)
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	body := string(buf[:n])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, banned := range []string{desc.BearerToken, d.Token(), "Bearer",
		"nativeSessionId", "api.token"} {
		if strings.Contains(body, banned) {
			t.Fatalf("leaked %q", banned)
		}
	}
}

// TestRuntimesAdminCookieAuth: an authenticated Web Admin cookie reads
// /v1/runtimes with no CSRF header — GET is a safe method, so the
// cookie-auth CSRF obligation does not apply.
func TestRuntimesAdminCookieAuth(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	cookie := setupAdmin(t, d, c, "admin", "correct-horse-battery")

	req, err := http.NewRequest(http.MethodGet, d.endpoint()+"/v1/runtimes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{
		Name:  adminCookieName,
		Value: cookie,
	})
	// Deliberately NO X-Relay-CSRF — safe methods must not need it.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin-cookie GET /v1/runtimes = %d", resp.StatusCode)
	}
	var out api.RuntimeList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.RuntimeCount != 0 {
		t.Fatalf("expected empty inventory: %+v", out.Totals)
	}
}
