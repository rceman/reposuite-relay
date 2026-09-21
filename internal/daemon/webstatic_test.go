package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
)

// TestEmbeddedBuildPresent: the committed SvelteKit build is inside the
// binary — index.html plus hashed _app assets — with no Node anywhere.
func TestEmbeddedBuildPresent(t *testing.T) {
	d, _ := webDaemon(t)
	for _, name := range []string{"index.html", "_app/version.json"} {
		if _, err := fs.Stat(d.static.fsys, name); err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
	}
	// At least one hashed JS entry must exist under _app/immutable.
	found := false
	err := fs.WalkDir(d.static.fsys, "_app/immutable", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() && strings.HasSuffix(p, ".js") {
			found = true
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("no embedded immutable JS asset (walk err %v)", err)
	}
}

// TestStaticRoutingPrecedence: /auth/* and /v1/* stay on the Go handlers;
// everything else is the SPA document or a real embedded asset.
func TestStaticRoutingPrecedence(t *testing.T) {
	d, c := webDaemon(t)
	endpoint := d.endpoint()

	// SPA document at / and a client-side route.
	for _, p := range []string{"/", "/sessions", "/deep/nested/route"} {
		resp, err := c.Get(endpoint + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "sveltekit") {
			t.Fatalf("%s: status %d, body %.80q", p, resp.StatusCode, body)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: Cache-Control %q", p, resp.Header.Get("Cache-Control"))
		}
	}

	// A real hashed asset serves its bytes with the immutable policy and
	// the right content type.
	var assetPath string
	err := fs.WalkDir(d.static.fsys, "_app/immutable", func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(p, ".js") && assetPath == "" {
			assetPath = p
		}
		return err
	})
	if err != nil || assetPath == "" {
		t.Fatal("no immutable js asset")
	}
	resp, err := c.Get(endpoint + "/" + assetPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("asset %s: %d len=%d", assetPath, resp.StatusCode, len(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("asset content-type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control %q", cc)
	}

	// Missing hashed assets and extension-bearing paths are real 404s,
	// never the SPA document.
	for _, p := range []string{
		"/_app/immutable/chunks/missing.ABCDEFGH.js",
		"/favicon.ico",
		"/_app/missing",
	} {
		resp, err := c.Get(endpoint + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", p, resp.StatusCode)
		}
	}

	// Traversal attempts fail closed — never normalized into a document.
	for _, p := range []string{"/../etc/passwd", "/%2e%2e/%2e%2e/etc/passwd", "/_app/../secret"} {
		resp, err := c.Get(endpoint + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("traversal %s served", p)
		}
	}

	// /auth/* and /v1/* never reach the SPA fallback.
	resp, err = c.Get(endpoint + "/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("/auth/session content-type %q", ct)
	}
	resp, err = c.Get(endpoint + "/v1/daemon")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /v1/daemon: %d", resp.StatusCode)
	}
	resp, err = c.Get(endpoint + "/v1/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous unknown /v1: %d", resp.StatusCode)
	}
	// Unknown /auth route stays a JSON 404, not the SPA.
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	resp, err = c.Get(endpoint + "/auth/bogus")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/auth/bogus: %d", resp.StatusCode)
	}
}

// TestStaticCSP: the document policy is hash-based — script-src 'self'
// plus a sha256 per inline block from the actual index.html, and never
// unsafe-inline or unsafe-eval.
func TestStaticCSP(t *testing.T) {
	d, _ := webDaemon(t)
	csp := d.static.csp
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:"} {
		if strings.Contains(csp, forbidden) {
			t.Fatalf("CSP contains %q: %s", forbidden, csp)
		}
	}
	for _, want := range []string{
		"script-src 'self'",
		"style-src 'self'",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	// The inline SvelteKit bootstrap must be hash-covered: every inline
	// <script> in index.html has a sha256 in the policy.
	inline := 0
	for _, m := range inlineBlock["script"].FindAllSubmatch(d.static.index, -1) {
		if len(m[1]) == 0 { // no attributes → inline body needs a hash
			inline++
			h := cspHash(m[3])
			if !strings.Contains(csp, h) {
				t.Fatalf("inline script hash %s not in CSP", h)
			}
		}
	}
	if inline == 0 {
		t.Fatal("expected the SvelteKit bootstrap inline script")
	}
}

// TestStaticMethodGate: non-GET/HEAD on the web surface is 405.
func TestStaticMethodGate(t *testing.T) {
	d, c := webDaemon(t)
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequest(m, d.endpoint()+"/sessions", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s /sessions: %d", m, resp.StatusCode)
		}
	}
}

// TestDashboardReadsDoNotWake: cookie-authenticated GET /v1/sessions
// returns the dashboard data while COLD sessions stay cold — the read is
// a pure observer and never starts a harness runtime.
func TestDashboardReadsDoNotWake(t *testing.T) {
	p := testPaths(t)
	// The codex availability check runs against the deterministic fake
	// app-server binary; create itself is COLD and spawns nothing.
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
	jar, _ := cookiejarForTest()
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := d.endpoint()
	token := setupAdmin(t, d, c, "admin", "correct-horse-12")
	sess, _, ok := d.web.cookieSession(mustCookieReq(t, endpoint, token))
	if !ok {
		t.Fatal("session not live")
	}
	// Two durable COLD sessions — create is runtime-free by contract, so
	// no codex binary is ever spawned.
	for _, key := range []string{"web-a", "web-b"} {
		body := fmt.Sprintf(`{"key":%q,"cwd":"/"}`, key)
		req := cookieRequest(t, http.MethodPost, endpoint, "/v1/sessions/codex",
			token, sess.CSRFToken, endpoint)
		req.Body = io.NopCloser(strings.NewReader(body))
		req.ContentLength = int64(len(body))
		resp := doReq(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create %s: %d", key, resp.StatusCode)
		}
	}
	// The dashboard read: cookie-authenticated GET, no CSRF needed.
	resp := doReq(t, cookieRequest(t, http.MethodGet, endpoint, "/v1/sessions", token, "", ""))
	var list struct {
		Daemon struct {
			APIVersion     int `json:"apiVersion"`
			SessionCount   int `json:"sessionCount"`
			ActiveSessions int `json:"activeSessions"`
			ColdSessions   int `json:"coldSessions"`
		} `json:"daemon"`
		Sessions []struct {
			Key          string `json:"key"`
			Harness      string `json:"harness"`
			RuntimeState string `json:"runtimeState"`
			Activity     string `json:"activity"`
			Generation   int    `json:"generation"`
			PID          int    `json:"pid"`
			Cwd          string `json:"cwd"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/sessions: %d", resp.StatusCode)
	}
	if list.Daemon.APIVersion != api.Version || list.Daemon.SessionCount != 2 ||
		list.Daemon.ColdSessions != 2 || list.Daemon.ActiveSessions != 0 {
		t.Fatalf("daemon projection wrong: %+v", list.Daemon)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("sessions %d", len(list.Sessions))
	}
	// Every session stayed COLD with zero runtime resources — the read
	// woke nothing.
	for _, s := range list.Sessions {
		if s.RuntimeState != "cold" || s.PID != 0 || s.Generation != 0 {
			t.Fatalf("session %s was woken by a read: %+v", s.Key, s)
		}
	}
}
