package daemon_test

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaleDescriptorRecovery: a crashed daemon leaves a stale descriptor
// (dead endpoint). A managed command spawns a contender; the lock owner
// atomically replaces the descriptor with its own generation.
func TestStaleDescriptorRecovery(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	stale := api.Descriptor{
		Version:     api.DescriptorVersion,
		InstanceID:  strings.Repeat("a", 32),
		PID:         999999,
		Endpoint:    "http://127.0.0.1:1", // nothing listens here
		BearerToken: strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(p.DaemonDescriptor(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// A managed command must recover and start the daemon.
	mustCLI(t, home, root, "list")
	_, count := daemonStatus(t, home, root)
	_ = count
	fresh := readDescriptor(t, home, root)
	if fresh.InstanceID == stale.InstanceID {
		t.Fatal("stale descriptor was not replaced by the lock owner")
	}
	stopDaemon(t, home, root)
}

// TestMalformedDescriptorFailsClosed: a corrupt descriptor is never
// trusted and never silently deleted — the client fails closed as a bad
// peer rather than sending commands somewhere unknown.
func TestMalformedDescriptorFailsClosed(t *testing.T) {
	home, root := testRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	marker := []byte("{not a descriptor")
	if err := os.WriteFile(p.DaemonDescriptor(), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, home, root, "list")
	if err == nil {
		t.Fatalf("malformed descriptor accepted: %s", out)
	}
	if !strings.Contains(out, "compatible relayd") {
		t.Fatalf("want fail-closed bad-peer error, got: %s", out)
	}
	got, _ := os.ReadFile(p.DaemonDescriptor())
	if string(got) != string(marker) {
		t.Fatal("client modified/deleted the descriptor")
	}
	// Non-loopback endpoint also fails closed.
	bad := api.Descriptor{
		Version: api.DescriptorVersion, InstanceID: strings.Repeat("a", 32),
		PID: 1234, Endpoint: "http://10.0.0.1:9000", BearerToken: strings.Repeat("b", 64),
	}
	raw, _ := json.Marshal(bad)
	os.WriteFile(p.DaemonDescriptor(), raw, 0o600)
	out, err = cli(t, home, root, "list")
	if err == nil || !strings.Contains(out, "loopback") {
		t.Fatalf("non-loopback descriptor accepted: %s %v", out, err)
	}
}

// TestBadRequestShapes: structurally valid JSON, semantically bad —
// stable machine codes on both path and body key validation.
func TestBadRequestShapes(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "ok")
	d := readDescriptor(t, home, root)

	status, out := rawReq(t, d, http.MethodGet, "/v1/sessions/missing", "", true)
	if status != 404 || !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("missing: %d %s", status, out)
	}
	status, out = rawReq(t, d, http.MethodDelete, "/v1/sessions/missing", "", true)
	if status != 404 || !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("stop missing: %d %s", status, out)
	}
	// Path-based key validation (single segment, URL-escaped space).
	status, out = rawReq(t, d, http.MethodGet, "/v1/sessions/bad%20key", "", true)
	if status != 400 || !strings.Contains(out, "INVALID_SESSION_KEY") {
		t.Fatalf("bad path key: %d %s", status, out)
	}
	// Path traversal is never a valid session route.
	status, _ = rawReq(t, d, http.MethodGet, "/v1/sessions/..%2Fx", "", true)
	if status != 404 {
		t.Fatalf("traversal route: %d", status)
	}
	// Duplicate create is a conflict.
	status, out = rawReq(t, d, http.MethodPost, "/v1/sessions/fixture", `{"key":"ok","cwd":"/"}`, true)
	if status != 409 || !strings.Contains(out, "SESSION_EXISTS") {
		t.Fatalf("duplicate: %d %s", status, out)
	}
	stopDaemon(t, home, root)
}

// TestPermissions: private modes on state dirs, descriptor, lock.
func TestPermissions(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "perm")
	defer stopDaemon(t, home, root)

	check := func(path string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Fatalf("%s mode %o want %o", path, got, want)
		}
	}
	check(filepath.Join(root, "relay"), 0o700)
	check(filepath.Join(root, "relay", "run"), 0o700)
	check(filepath.Join(root, "relay", "logs"), 0o700)
	check(filepath.Join(root, "relay", "run", "daemon.json"), 0o600)
	check(filepath.Join(root, "relay", "run", "relayd.lock"), 0o600)
	check(filepath.Join(root, "relay", "sessions"), 0o700)
}

// TestRelativeHomeRejected: relative REPOSUITE_HOME fails before any
// daemon/socket is created.
func TestRelativeHomeRejected(t *testing.T) {
	home, _ := testRoot(t)
	for _, rel := range []string{"reposuite", "./rs", "x/y"} {
		out, err := cli(t, home, rel, "list")
		if err == nil {
			t.Fatalf("REPOSUITE_HOME=%q accepted: %s", rel, out)
		}
		if !strings.Contains(out, "not absolute") {
			t.Fatalf("REPOSUITE_HOME=%q wrong error: %s", rel, out)
		}
		if _, statErr := os.Stat(filepath.Join(home, rel, "relay")); !os.IsNotExist(statErr) {
			t.Fatalf("REPOSUITE_HOME=%q created state", rel)
		}
	}
}

// TestDaemonPidAndVersion: daemon status exposes pid + API version.
func TestDaemonPidAndVersion(t *testing.T) {
	home, root := testRoot(t)
	serveKey(t, home, root, "x")
	out := mustCLI(t, home, root, "status", "--json")
	var rep struct {
		PID        int `json:"pid"`
		APIVersion int `json:"apiVersion"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if rep.PID <= 1 || !alive(rep.PID) {
		t.Fatalf("daemon pid %d not alive", rep.PID)
	}
	if rep.APIVersion != api.Version {
		t.Fatalf("apiVersion=%d, want %d", rep.APIVersion, api.Version)
	}
	stopDaemon(t, home, root)
}
