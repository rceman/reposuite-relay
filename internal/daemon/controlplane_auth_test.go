package daemon

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// TestMachineTokenLifecycle: first start mints the persistent token,
// restarts keep it, the descriptor bearer rotates, and both authenticate.
func TestMachineTokenLifecycle(t *testing.T) {
	p := testPaths(t)
	d1, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	done1 := serveDaemon(t, d1)
	endpoint := d1.endpoint()

	tok1, err := auth.Load(p.MachineToken())
	if err != nil {
		t.Fatalf("first start must create the machine token: %v", err)
	}
	fi, err := os.Stat(p.MachineToken())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("api.token mode %o, want 0600", fi.Mode().Perm())
	}
	// The machine token is NEVER in the descriptor.
	rawDesc, _ := os.ReadFile(p.DaemonDescriptor())
	if strings.Contains(string(rawDesc), tok1) {
		t.Fatal("machine token leaked into daemon.json")
	}
	descTok1 := d1.Token()

	// Dual auth: machine bearer and descriptor bearer both authenticate.
	for name, bearer := range map[string]string{
		"machine":    tok1,
		"descriptor": descTok1,
	} {
		resp := rawGet(t, endpoint, "/v1/daemon", bearer)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s bearer: status %d, want 200", name, resp.StatusCode)
		}
	}
	resp := rawGet(t, endpoint, "/v1/daemon", "deadbeef")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad bearer: status %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(string(body), api.ErrUnauthorized) {
		t.Fatalf("bad bearer body %q missing UNAUTHORIZED", body)
	}
	resp = rawGet(t, endpoint, "/v1/daemon", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer: status %d, want 401", resp.StatusCode)
	}
	// The machine token is never in an API response.
	resp = rawGet(t, endpoint, "/v1/daemon", descTok1)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), tok1) || strings.Contains(string(body), descTok1) {
		t.Fatal("a credential leaked into an API response")
	}

	stopDaemonNow(t, d1, done1)

	// Restart: machine token persists exactly; descriptor bearer rotates.
	d2, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	tok2, err := auth.Load(p.MachineToken())
	if err != nil || tok2 != tok1 {
		t.Fatalf("machine token not persistent across restart: %v", err)
	}
	if d2.Token() == descTok1 {
		t.Fatal("descriptor bearer did not rotate across generations")
	}
	// The persisted machine token still authenticates on the new
	// generation.
	resp = rawGet(t, d2.endpoint(), "/v1/daemon", tok1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("machine bearer on restart: status %d", resp.StatusCode)
	}
	// The OLD descriptor bearer does not.
	resp = rawGet(t, d2.endpoint(), "/v1/daemon", descTok1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale descriptor bearer: status %d, want 401", resp.StatusCode)
	}
}

// TestMalformedMachineTokenFailsClosed: a corrupt api.token fails
// startup and is never silently replaced.
func TestMalformedMachineTokenFailsClosed(t *testing.T) {
	p := testPaths(t)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.MachineToken(), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
		t.Fatal("malformed api.token must fail startup")
	}
	after, _ := os.ReadFile(p.MachineToken())
	if string(after) != "corrupt\n" {
		t.Fatal("malformed token was silently replaced")
	}
}

// TestRootPathPlaceholder: GET / answers the embedded Web Admin SPA
// shell anonymously — no operational data, no credential material.
func TestRootPathPlaceholder(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	resp := rawGet(t, d.endpoint(), "/", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status %d", resp.StatusCode)
	}
	if !bytes.Equal(body, d.static.index) {
		t.Fatalf("GET / is not the embedded SPA entry: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / content-type %q", ct)
	}
	// Anonymous /v1 stays rejected even though / is open.
	resp = rawGet(t, d.endpoint(), "/v1/daemon", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /v1 status %d, want 401", resp.StatusCode)
	}
}

// TestInsecureConfigStateFailsClosed: pre-existing owner-exposed
// control-plane state fails startup — insecure relay.json, insecure
// api.token, or an insecure config directory. Nothing is repaired and no
// readiness descriptor is published.
func TestInsecureConfigStateFailsClosed(t *testing.T) {
	validCfg := `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":1}`
	tok, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T, p paths.Paths){
		"insecure-config-file": func(t *testing.T, p paths.Paths) {
			if err := os.WriteFile(p.RelayConfig(), []byte(validCfg), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"insecure-token-file": func(t *testing.T, p paths.Paths) {
			if err := os.WriteFile(p.MachineToken(), []byte(tok+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"insecure-config-dir": func(t *testing.T, p paths.Paths) {
			if err := os.Chmod(p.ConfigDir(), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		p := testPaths(t)
		if err := p.Ensure(); err != nil {
			t.Fatal(err)
		}
		plant(t, p)
		if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
			t.Fatalf("%s: insecure control-plane state must fail startup", name)
		}
		if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
			t.Fatalf("%s: readiness descriptor published for failed startup", name)
		}
	}
}
