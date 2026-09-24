package daemon

// Isolated acceptance against a REAL RepoDex service built at the accepted
// revision c2bd73a. The binary is supplied via REPODEX_TEST_BIN; without it
// the test skips — it never builds, installs, or starts anything itself.
// All state lives in t.TempDir(); the user's real ~/reposuite/repodex is
// never touched.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/client"
)

// repodexProc is a real `reposuite-repodex serve` child on a temp state dir.
type repodexProc struct {
	cmd *exec.Cmd
	dir string
}

func startRepoDex(t *testing.T, bin, stateDir string) *repodexProc {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--state-dir", stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start repodex: %v", err)
	}
	p := &repodexProc{
		cmd: cmd,
		dir: stateDir,
	}
	// Wait for the published descriptor — never a fixed port.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(stateDir, "service.runtime.json")); err == nil {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatal("repodex did not publish service.runtime.json")
	return nil
}

func (p *repodexProc) stop() {
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func (p *repodexProc) endpoint() (host string, port int, token string, err error) {
	raw, err := os.ReadFile(filepath.Join(p.dir, "service.runtime.json"))
	if err != nil {
		return "", 0, "", err
	}
	var d struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", 0, "", err
	}
	tok, err := os.ReadFile(filepath.Join(p.dir, "service.token"))
	if err != nil {
		return "", 0, "", err
	}
	return d.Host, d.Port, strings.TrimSpace(string(tok)), nil
}

// persistedEvents reads RepoDex /v1/status → events.persisted.
// Returns -1 while the service is unreachable (restart window) so pollers
// can distinguish outage from failure.
func (p *repodexProc) persistedEvents(t *testing.T) int64 {
	t.Helper()
	host, port, token, err := p.endpoint()
	if err != nil {
		return -1
	}
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s:%d/v1/status", host, port), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var st struct {
		Events struct {
			Persisted int64 `json:"persisted"`
		} `json:"events"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &st); err != nil {
		return -1
	}
	return st.Events.Persisted
}

// enableTelemetryConfig writes repodex.enabled into the existing relay.json.
func enableTelemetryConfig(t *testing.T, configPath, stateDir string) {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["repodex"] = map[string]any{
		"enabled": true, "stateDir": stateDir,
		"batchMaxDelayMs": 50,
	}
	out, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// relayTelemetryHealth fetches the daemon's telemetry snapshot.
func relayTelemetryHealth(t *testing.T, endpoint, bearer string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet,
		endpoint+"/v1/telemetry/repodex", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Telemetry map[string]any `json:"telemetry"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Telemetry
}

func telemetryNum(h map[string]any, key string) int64 {
	n, _ := h[key].(float64)
	return int64(n)
}

// TestRepoDexRealIngest: one Codex turn against a real RepoDex — canonical
// events durably persisted, health connected, zero loss.
func TestRepoDexRealIngest(t *testing.T) {
	bin := os.Getenv("REPODEX_TEST_BIN")
	if bin == "" {
		t.Skip("REPODEX_TEST_BIN not set")
	}
	stateDir := filepath.Join(t.TempDir(), "repodex")
	rd := startRepoDex(t, bin, stateDir)
	defer rd.stop()

	p := testPaths(t)
	d, _, stop := startDaemonGeneration(t, p, Options{SelfExe: testBinary()})
	d.Shutdown()
	stop()
	enableTelemetryConfig(t, p.RelayConfig(), stateDir)
	d2, c2, stop2 := startDaemonGeneration(t, p, withCodex())
	defer stop2()
	_ = d2

	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	m := serveCodex(t, c2, "s1")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c2.Prompt(ctx, m.Key, "tool:read", "", ""); err != nil {
		t.Fatal(err)
	}
	// Expected canonical stream: session_started, tool_call_started,
	// tool_call_completed, source_observed, final_answer,
	// model_call_completed → 6 events.
	waitForDaemon(t, "repodex persisted", func() bool {
		return rd.persistedEvents(t) >= 6
	})
	h := relayTelemetryHealth(t, c2.Endpoint(), desc.BearerToken)
	if h["state"] != "connected" {
		t.Fatalf("telemetry state: %v", h)
	}
	if lost := telemetryNum(h, "lost"); lost != 0 {
		t.Fatalf("events lost: %d", lost)
	}
	if got := telemetryNum(h, "acknowledged") + telemetryNum(h, "duplicates"); got < 6 {
		t.Fatalf("acked+dup < 6: %d", got)
	}
}

// TestRepoDexRestartRediscovery: RepoDex restart rebinds a NEW dynamic port;
// Relay rediscovers the descriptor and replays the outage spool with zero
// loss — identical event_ids deduped server-side.
func TestRepoDexRestartRediscovery(t *testing.T) {
	bin := os.Getenv("REPODEX_TEST_BIN")
	if bin == "" {
		t.Skip("REPODEX_TEST_BIN not set")
	}
	stateDir := filepath.Join(t.TempDir(), "repodex")
	rd := startRepoDex(t, bin, stateDir)

	p := testPaths(t)
	d, _, stop := startDaemonGeneration(t, p, Options{SelfExe: testBinary()})
	d.Shutdown()
	stop()
	enableTelemetryConfig(t, p.RelayConfig(), stateDir)
	d2, c2, stop2 := startDaemonGeneration(t, p, withCodex())
	defer stop2()
	_ = d2
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	m := serveCodex(t, c2, "s1")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c2.Prompt(ctx, m.Key, "tool:read", "", ""); err != nil {
		t.Fatal(err)
	}
	waitForDaemon(t, "pre-outage persisted", func() bool {
		return rd.persistedEvents(t) >= 6
	})
	// OUTAGE: kill RepoDex, prompt again — events must spool, not drop.
	rd.stop()
	if _, err := c2.Prompt(ctx, m.Key, "tool:list", "", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // let the sender observe the outage
	rd2 := startRepoDex(t, bin, stateDir)
	defer rd2.stop()
	// Restarted on a new dynamic port — Relay must rediscover + replay.
	waitForDaemon(t, "post-restart persisted", func() bool {
		return rd2.persistedEvents(t) >= 10
	})
	h := relayTelemetryHealth(t, c2.Endpoint(), desc.BearerToken)
	if lost := telemetryNum(h, "lost"); lost != 0 {
		t.Fatalf("events lost across outage: %d", lost)
	}
	if telemetryNum(h, "rediscoveries") == 0 {
		t.Fatal("no rediscovery recorded after RepoDex restart")
	}
}

// TestRepoDexTenSessions: 10 concurrent sessions, zero cross-session
// contamination, zero loss. Every session's canonical stream is disjoint
// by session_id — RepoDex event_ids prove it.
func TestRepoDexTenSessions(t *testing.T) {
	bin := os.Getenv("REPODEX_TEST_BIN")
	if bin == "" {
		t.Skip("REPODEX_TEST_BIN not set")
	}
	stateDir := filepath.Join(t.TempDir(), "repodex")
	rd := startRepoDex(t, bin, stateDir)
	defer rd.stop()

	p := testPaths(t)
	d, _, stop := startDaemonGeneration(t, p, Options{SelfExe: testBinary()})
	d.Shutdown()
	stop()
	enableTelemetryConfig(t, p.RelayConfig(), stateDir)
	d2, c2, stop2 := startDaemonGeneration(t, p, withCodex())
	defer stop2()
	_ = d2
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("s%d", i)
			m := serveCodex(t, c2, key)
			ctx, cancel := tctx(t)
			defer cancel()
			if _, err := c2.Prompt(ctx, key, "tool:read", "", ""); err != nil {
				errs[i] = err
				return
			}
			_ = m
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	waitForDaemon(t, "10-session persisted", func() bool {
		return rd.persistedEvents(t) >= 6*n
	})
	h := relayTelemetryHealth(t, c2.Endpoint(), desc.BearerToken)
	if lost := telemetryNum(h, "lost"); lost != 0 {
		t.Fatalf("lost: %d", lost)
	}
	if rej := telemetryNum(h, "rejected"); rej != 0 {
		t.Fatalf("rejected (cross-contamination symptom): %d", rej)
	}
}

// withCodex builds daemon Options pointed at the deterministic fake.
func withCodex() Options {
	o := Options{SelfExe: testBinary()}
	codexOptions(&o)
	return o
}

func waitForDaemon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}
