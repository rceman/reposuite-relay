package daemon

// GET /v1/telemetry/repodex — observer-only RepoDex telemetry health.
// Never wakes a session, starts RepoDex, or exposes credentials.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/client"
)

func rawGetTelemetry(t *testing.T, endpoint, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		endpoint+"/v1/telemetry/repodex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Auth matrix: anonymous 401, descriptor bearer 200, machine bearer 200.
// Telemetry disabled by default — Relay works with zero RepoDex dependency.
func TestTelemetryHealthAuth(t *testing.T) {
	d, p, c, served := startInProcess(t, nil)
	_ = d
	_ = c
	_ = served
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := rawGetTelemetry(t, desc.Endpoint, ""); code != 401 {
		t.Fatalf("anonymous = %d", code)
	}
	code, body := rawGetTelemetry(t, desc.Endpoint, desc.BearerToken)
	if code != 200 {
		t.Fatalf("descriptor bearer = %d: %s", code, body)
	}
	var v struct {
		Telemetry struct {
			Enabled bool   `json:"enabled"`
			State   string `json:"state"`
		} `json:"telemetry"`
		Daemon map[string]any `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Telemetry.Enabled || v.Telemetry.State != "disabled" {
		t.Fatalf("telemetry shape: %s", body)
	}
	if v.Daemon == nil {
		t.Fatal("daemon info absent")
	}
	tok, err := os.ReadFile(p.MachineToken())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := rawGetTelemetry(t, desc.Endpoint,
		strings.TrimSpace(string(tok))); code != 200 {
		t.Fatalf("machine bearer = %d", code)
	}
	// No credential material anywhere in the body.
	for _, bad := range []string{"service.token", "Bearer ", "authorization"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
			t.Fatalf("credential material in response: %q", bad)
		}
	}
}

// Enabled config end-to-end: a repodex-enabled relay.json activates the
// sender on the next generation; with no RepoDex running the service stays
// disconnected and the agent path is unaffected.
func TestTelemetryEnabledConfig(t *testing.T) {
	p := testPaths(t)
	// First generation establishes the stable endpoint.
	d, c, stop := startDaemonGeneration(t, p, Options{SelfExe: testBinary()})
	_ = c
	d.Shutdown()
	stop()
	// Enable telemetry via relay.json for the next generation.
	raw, err := os.ReadFile(p.RelayConfig())
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["repodex"] = map[string]any{
		"enabled":  true,
		"stateDir": filepath.Join(t.TempDir(), "repodex"),
	}
	out, _ := json.Marshal(cfg)
	if err := os.WriteFile(p.RelayConfig(), out, 0o600); err != nil {
		t.Fatal(err)
	}
	d2, c2, stop2 := startDaemonGeneration(t, p, Options{SelfExe: testBinary()})
	defer stop2()
	_ = d2
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	code, body := rawGetTelemetry(t, c2.Endpoint(), desc.BearerToken)
	if code != 200 {
		t.Fatalf("enabled daemon telemetry endpoint = %d", code)
	}
	var v struct {
		Telemetry struct {
			Enabled bool   `json:"enabled"`
			State   string `json:"state"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Telemetry.Enabled {
		t.Fatalf("telemetry not enabled: %s", body)
	}
	// No RepoDex → disconnected or degraded, never healthy-faked.
	if v.Telemetry.State != "disconnected" && v.Telemetry.State != "degraded" {
		t.Fatalf("unexpected state: %s", v.Telemetry.State)
	}
}
