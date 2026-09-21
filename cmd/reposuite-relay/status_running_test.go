package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/daemon"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// TestStatusRunning: a live daemon reports RUNNING with pid, uptime,
// session counts — and no credential anywhere in the output.
func TestStatusRunning(t *testing.T) {
	bin := buildBinary(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.Start(p, daemon.Options{SelfExe: bin})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = d.Serve(); close(done) }()
	defer func() {
		d.Shutdown()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Error("daemon did not finish shutdown")
		}
	}()
	var c *client.Client
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cl, err := client.Dial(p); err == nil {
			c = cl
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("daemon did not become ready")
	}
	machineTok, err := auth.Load(p.MachineToken())
	if err != nil {
		t.Fatal(err)
	}

	out, err := runStatus(t, bin, root, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"RUNNING", "PID:", "Uptime:", "Sessions:", "Active:", "Cold:", "API auth:     configured"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, machineTok) || strings.Contains(out, d.Token()) {
		t.Fatal("status output leaked a credential")
	}

	out, err = runStatus(t, bin, root, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Running       bool    `json:"running"`
		Status        string  `json:"status"`
		PID           int     `json:"pid"`
		Sessions      int     `json:"sessions"`
		APIVersion    int     `json:"apiVersion"`
		UptimeSeconds float64 `json:"uptimeSeconds"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if !rep.Running || rep.Status != "running" || rep.PID <= 1 || rep.UptimeSeconds <= 0 {
		t.Fatalf("unexpected running report: %s", out)
	}
	if rep.PID != os.Getpid() {
		t.Fatalf("status pid %d != daemon pid %d", rep.PID, os.Getpid())
	}
	if strings.Contains(out, machineTok) || strings.Contains(out, d.Token()) {
		t.Fatal("status --json leaked a credential")
	}

	// Stop → status flips to STOPPED with the configured endpoint intact.
	d.Shutdown()
	<-done
	out, err = runStatus(t, bin, root, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "STOPPED") || !strings.Contains(out, "127.0.0.1:") {
		t.Fatalf("post-stop status wrong:\n%s", out)
	}
}

// TestStatusArgValidation: the product status takes zero positional
// args — the old `status <KEY>` shape moved to `session status`.
func TestStatusArgValidation(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	if out, err := runStatus(t, bin, root, "status", "some-key"); err == nil {
		t.Fatalf("status <KEY> must be rejected, got %q", out)
	}
	if out, err := runStatus(t, bin, root, "status", "--bogus"); err == nil {
		t.Fatalf("status --bogus must be rejected, got %q", out)
	}
}

// TestSessionStatusNamespace: `session status <KEY>` carries the old
// per-session diagnostics (auto-starting the daemon like any managed
// command).
func TestSessionStatusNamespace(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	// Session status on an empty root auto-starts (managed command) and
	// then reports the unknown key.
	out, err := runStatus(t, bin, root, "session", "status", "missing")
	if err == nil {
		t.Fatalf("session status of unknown key must fail: %s", out)
	}
	if !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("expected SESSION_NOT_FOUND: %s", out)
	}
	// The auto-start means a daemon IS now running — product status sees it.
	out, err = runStatus(t, bin, root, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "RUNNING") {
		t.Fatalf("expected RUNNING after managed auto-start:\n%s", out)
	}
	if _, err := runStatus(t, bin, root, "daemon", "stop"); err != nil {
		t.Fatalf("daemon stop: %v: %s", err, out)
	}
}

// TestStatusInvalidTokenSurfaced: a present-but-malformed or
// owner-exposed api.token reports an explicit invalid auth state — never
// masked as "not configured", never repaired, never printed.
func TestStatusInvalidTokenSurfaced(t *testing.T) {
	bin := buildBinary(t)
	for name, plant := range map[string]func(t *testing.T, p paths.Paths) string{
		"malformed": func(t *testing.T, p paths.Paths) string {
			raw := "not-a-token"
			if err := os.WriteFile(p.MachineToken(), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			return raw
		},
		"insecure-mode": func(t *testing.T, p paths.Paths) string {
			tok, err := auth.Generate()
			if err != nil {
				t.Fatal(err)
			}
			raw := tok + "\n"
			if err := os.WriteFile(p.MachineToken(), []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			return raw
		},
	} {
		base := t.TempDir()
		home := filepath.Join(base, "home")
		root := filepath.Join(base, "rs")
		for _, dir := range []string{home, root} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		p, err := paths.Resolve(home, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Ensure(); err != nil {
			t.Fatal(err)
		}
		raw := plant(t, p)

		out, err := runStatus(t, bin, root, "status")
		if err != nil {
			t.Fatalf("%s: status: %v\n%s", name, err, out)
		}
		if !strings.Contains(out, "INVALID") {
			t.Fatalf("%s: invalid token not surfaced:\n%s", name, out)
		}
		out, err = runStatus(t, bin, root, "status", "--json")
		if err != nil {
			t.Fatalf("%s: status --json: %v\n%s", name, err, out)
		}
		var rep struct {
			APIAuthConfigured bool   `json:"apiAuthConfigured"`
			APIAuthStatus     string `json:"apiAuthStatus"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%s: parse: %v\n%s", name, err, out)
		}
		if rep.APIAuthConfigured || rep.APIAuthStatus != "invalid" {
			t.Fatalf("%s: want apiAuthStatus=invalid, got %s", name, out)
		}
		if strings.Contains(out, raw) {
			t.Fatalf("%s: status leaked credential bytes", name)
		}
		// Status is diagnostic only: the corrupt file is byte-identical.
		after, _ := os.ReadFile(p.MachineToken())
		if string(after) != raw {
			t.Fatalf("%s: status mutated the credential file", name)
		}
	}
}

// TestStatusAdminAuthStates: adminAuthStatus tracks the admin.json state
// exactly — missing (setup mode), configured, or invalid — and never
// prints the username, hash, or any token.
func TestStatusAdminAuthStates(t *testing.T) {
	bin := buildBinary(t)
	hash, err := adminauth.Hash("a-valid-test-password", adminauth.TestParams)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"schemaVersion":1,"username":"admin","passwordHash":"` + hash + `"}`
	for name, plant := range map[string]struct {
		body     string
		mode     os.FileMode
		wantJSON string
		wantText string
	}{
		"missing":    {"", 0, "missing", "not configured"},
		"configured": {valid, 0o600, "configured", "configured"},
		"invalid":    {`{"schemaVersion":1,"username":"a","passwordHash":"x"}`, 0o600, "invalid", "INVALID"},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "home")
			root := filepath.Join(base, "rs")
			for _, dir := range []string{home, root} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			p, err := paths.Resolve(home, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Ensure(); err != nil {
				t.Fatal(err)
			}
			if plant.body != "" {
				if err := os.WriteFile(p.AdminCredentials(), []byte(plant.body), plant.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p.AdminCredentials(), plant.mode); err != nil {
					t.Fatal(err)
				}
			}
			out, err := runStatus(t, bin, root, "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			if !strings.Contains(out, "Admin auth:   "+plant.wantText) {
				t.Fatalf("human status missing %q:\n%s", plant.wantText, out)
			}
			out, err = runStatus(t, bin, root, "status", "--json")
			if err != nil {
				t.Fatalf("status --json: %v\n%s", err, out)
			}
			var rep struct {
				AdminAuthStatus     string `json:"adminAuthStatus"`
				AdminAuthConfigured bool   `json:"adminAuthConfigured"`
			}
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("parse: %v\n%s", err, out)
			}
			if rep.AdminAuthStatus != plant.wantJSON ||
				rep.AdminAuthConfigured != (plant.wantJSON == "configured") {
				t.Fatalf("want %s, got %s", plant.wantJSON, out)
			}
			for _, leak := range []string{"a-valid-test-password", hash} {
				if strings.Contains(out, leak) {
					t.Fatalf("status leaked admin credential material")
				}
			}
		})
	}
}
