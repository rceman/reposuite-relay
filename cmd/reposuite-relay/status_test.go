package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/daemon"
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
		Running    bool   `json:"running"`
		Configured bool   `json:"configured"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("status --json parse: %v\n%s", err, out)
	}
	if rep.Running || rep.Configured || rep.Status != "stopped" {
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
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if rep.Running || rep.Status != "stopped" || !rep.Configured ||
		rep.Address != "127.0.0.1:17432" || !rep.APIAuthConfigured {
		t.Fatalf("unexpected report: %s", out)
	}
	if strings.Contains(out, tok) {
		t.Fatal("status --json leaked the machine token")
	}
}

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
