package daemon_test

import (
	"github.com/rceman/reposuite-relay/internal/api"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMalformedClient: every bad input gets a bounded response; the daemon
// stays healthy throughout. Auth is required on every endpoint.
func TestMalformedClient(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "alive-check")
	d := readDescriptor(t, home, root)

	pingOK := func() {
		status, out := rawReq(t, d, http.MethodGet, "/v1/daemon", "", true)
		if status != 200 || !strings.Contains(out, `"apiVersion":`+strconv.Itoa(api.Version)) {
			t.Fatalf("daemon unhealthy after malformed input: %d %s", status, out)
		}
	}

	// Every endpoint requires the bearer token.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/daemon", ""},
		{http.MethodGet, "/v1/sessions", ""},
		{http.MethodGet, "/v1/sessions/alive-check", ""},
		{http.MethodDelete, "/v1/sessions/alive-check", ""},
		{http.MethodGet, "/v1/sessions/alive-check/transcript", ""},
		{http.MethodGet, "/v1/sessions/alive-check/events", ""},
		{http.MethodPost, "/v1/daemon/shutdown", ""},
		{http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/"}`},
	} {
		status, out := rawReq(t, d, tc.method, tc.path, tc.body, false)
		if status != 401 || !strings.Contains(out, "UNAUTHORIZED") {
			t.Fatalf("%s %s without auth: %d %s", tc.method, tc.path, status, out)
		}
	}
	// Wrong token is also 401.
	req, _ := http.NewRequest(http.MethodGet, d.Endpoint+"/v1/daemon", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("0", 64))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("wrong token: %d", resp.StatusCode)
		}
	}
	pingOK()

	// Malformed JSON body.
	status, out := rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", "{not json", true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("malformed json: %d %s", status, out)
	}
	pingOK()

	// Unknown field / trailing value.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/","exec":"rm"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("unknown field: %d %s", status, out)
	}
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"x","cwd":"/"} {"key":"y"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("trailing value: %d %s", status, out)
	}
	// Relative cwd.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"rel","cwd":"relative/path"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("relative cwd: %d %s", status, out)
	}
	// Invalid key.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"bad key!","cwd":"/"}`, true)
	if status != 400 || !strings.Contains(out, "INVALID_SESSION_KEY") {
		t.Fatalf("bad key: %d %s", status, out)
	}
	pingOK()

	// Oversized body (> 64 KiB).
	big := `{"key":"` + strings.Repeat("A", 70*1024) + `","cwd":"/"}`
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", big, true)
	if status != 400 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("oversized: %d %.200s", status, out)
	}
	pingOK()

	// Unknown route / wrong method.
	status, _ = rawReq(t, d, http.MethodGet, "/v1/nope", "", true)
	if status != 404 {
		t.Fatalf("unknown route: %d", status)
	}
	status, out = rawReq(t, d, http.MethodPatch, "/v1/sessions/alive-check", "", true)
	if status != 405 || !strings.Contains(out, "INVALID_REQUEST") {
		t.Fatalf("method not allowed: %d %s", status, out)
	}
	pingOK()

	// Idle client: connect, send nothing — daemon closes it via the header
	// deadline (never delays shutdown for it either).
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(d.Endpoint, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection was not closed by deadline")
	}
	conn.Close()
	pingOK()

	stopDaemon(t, home, root)
}
