// Dashboard observer regression: the Web Admin reads must never wake a
// COLD session or spawn a harness runtime.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
)

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
