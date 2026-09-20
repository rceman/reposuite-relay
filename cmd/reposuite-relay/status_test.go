package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// buildBinary compiles the production binary once per test — the real
// `status` surface is exercised end-to-end.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "reposuite-relay")
	if out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// runStatus executes `status` against an isolated root.
func runStatus(t *testing.T, bin, root string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "REPOSUITE_HOME="+root)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestStatusNeverInitialized: status before any daemon run — STOPPED,
// not configured, and NOTHING is created (no config, no descriptor, no
// daemon spawned).
func TestStatusNeverInitialized(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	out, err := runStatus(t, bin, root, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"Status:", "STOPPED", "not configured", "API auth:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q missing %q", out, want)
		}
	}
	// Hard no-auto-start proof: status may not create ANY state.
	if _, err := os.Stat(filepath.Join(root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("status created relay state:\n%s", out)
	}
	// --json reports the same shape machine-side.
	out, err = runStatus(t, bin, root, "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	var rep struct {
		Running       bool   `json:"running"`
		Configured    bool   `json:"configured"`
		Status        string `json:"status"`
		APIAuthStatus string `json:"apiAuthStatus"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json parse: %v\n%s", err, out)
	}
	if rep.Running || rep.Configured || rep.Status != "stopped" || rep.APIAuthStatus != "missing" {
		t.Fatalf("unexpected report: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "relay")); !os.IsNotExist(err) {
		t.Fatal("status --json created relay state")
	}
}

// TestStatusConfiguredStopped: config + token present, daemon down —
// the stable endpoint and auth state are reported from config alone.
func TestStatusConfiguredStopped(t *testing.T) {
	bin := buildBinary(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := paths.Resolve(home, filepath.Join(root, "rs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{SchemaVersion: config.SchemaVersion, ListenHost: config.ListenHost, ListenPort: 17432}
	if err := config.Commit(p.RelayConfig(), cfg); err != nil {
		t.Fatal(err)
	}
	tok, err := auth.Ensure(p.MachineToken())
	if err != nil {
		t.Fatal(err)
	}
	out, err := runStatus(t, bin, filepath.Join(root, "rs"), "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"STOPPED", "127.0.0.1:17432",
		"http://127.0.0.1:17432/", "http://127.0.0.1:17432/v1", "API auth:     configured"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, tok) {
		t.Fatal("status output leaked the machine token")
	}
	// --json: configured endpoint without runtime fields.
	out, err = runStatus(t, bin, filepath.Join(root, "rs"), "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Running           bool   `json:"running"`
		Configured        bool   `json:"configured"`
		Status            string `json:"status"`
		Address           string `json:"address"`
		APIAuthConfigured bool   `json:"apiAuthConfigured"`
		APIAuthStatus     string `json:"apiAuthStatus"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if rep.Running || rep.Status != "stopped" || !rep.Configured ||
		rep.Address != "127.0.0.1:17432" || !rep.APIAuthConfigured ||
		rep.APIAuthStatus != "configured" {
		t.Fatalf("unexpected report: %s", out)
	}
	if strings.Contains(out, tok) {
		t.Fatal("status --json leaked the machine token")
	}
}
