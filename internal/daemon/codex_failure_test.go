package daemon

import (
	"encoding/json"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/session"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCodexRuntimeDeathAndRecoveryOverHTTP: a runtime crash fails the
// in-flight turn durably, drops the session COLD, and the next prompt
// resumes the exact native session (when one was materialized).
func TestCodexRuntimeDeathAndRecoveryOverHTTP(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "crash")
	ctx, cancel := tctx(t)
	defer cancel()

	if _, err := c.Prompt(ctx, "crash", "first", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "crash", api.EventMessageAgentCompleted)
	st, err := c.Status(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	native := st.Session.NativeSessionID
	if native == "" {
		t.Fatal("session not materialized")
	}

	// The next generation's app-server dies with a turn in flight. The
	// submission may be accepted before the crash (the harness answered
	// first), so the contract under test is the durable failure record,
	// not the HTTP status.
	t.Setenv("FAKE_CODEX_MODE", "die-on-turn")
	if err := d.supervisor.Stop(codex.RuntimeKey); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	if d.supervisor.Len() != 0 {
		t.Fatal("runtime must be gone")
	}
	_, _ = c.Prompt(ctx, "crash", "second", "", "")
	waitDurableTypes(t, c, "crash", api.EventRuntimeExited)
	waitDurableTypes(t, c, "crash", api.EventTurnFailed)
	if d.supervisor.Len() != 0 {
		t.Fatalf("dead runtime still registered: %d", d.supervisor.Len())
	}

	// Recovery: a fresh generation resumes the exact same native thread.
	t.Setenv("FAKE_CODEX_MODE", "happy")
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := c.Prompt(ctx, "crash", "third", "", "")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery prompt kept failing: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitDurableTypes(t, c, "crash", api.EventMessageAgentCompleted)
	page, err := c.Transcript(ctx, "crash", 200)
	if err != nil {
		t.Fatal(err)
	}
	resumed := 0
	for _, r := range page.Records {
		if r.Type != api.EventHarnessStarted {
			continue
		}
		var started struct {
			NativeSessionID string `json:"nativeSessionId"`
			Resumed         bool   `json:"resumed"`
		}
		if err := json.Unmarshal(r.Payload, &started); err != nil {
			t.Fatal(err)
		}
		if started.NativeSessionID != native {
			t.Fatalf("thread %q, want the exact %q", started.NativeSessionID, native)
		}
		if started.Resumed {
			resumed++
		}
	}
	// Every generation after the first must reattach the exact thread.
	if resumed < 1 {
		t.Fatalf("resume records = %d, want at least 1", resumed)
	}
	st, err = c.Status(ctx, "crash")
	if err != nil {
		t.Fatal(err)
	}
	// Generation counts bindings: create 0, first bind 1, the doomed
	// resume-bind 2, the recovery resume-bind 3.
	if st.Session.NativeSessionID != native || st.Session.Generation != 3 {
		t.Fatalf("identity/generation drifted: %+v", st.Session)
	}
}

// TestCodexMissingHarnessFailsClosed: creating a Codex session on a host
// without the harness fails closed with a stable code.
func TestCodexMissingHarnessFailsClosed(t *testing.T) {
	_, _, c, _ := startInProcess(t, func(o *Options) {
		o.CodexCommand = func() (codex.Command, error) {
			return codex.Command{}, os.ErrNotExist
		}
	})
	ctx, cancel := tctx(t)
	defer cancel()
	_, err := c.CreateSession(ctx, session.HarnessCodex, "nope", t.TempDir(), "", "")
	if err == nil {
		t.Fatal("create must fail without the harness")
	}
	wantAPIErr(t, err, api.ErrRuntimeUnavailable)
	// The key stays free.
	if _, err := c.CreateSession(ctx, session.HarnessCodex, "nope", t.TempDir(), "", ""); err == nil {
		t.Fatal("expected failure again")
	}
}

// TestCodexFixtureSessionsUnaffected: the fixture harness keeps working
// alongside the Codex adapter (no shared-state regressions).
func TestCodexFixtureSessionsUnaffected(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	d, _, c, _ := startInProcess(t, codexOptions)
	serveCodex(t, c, "native")
	if _, err := serve(t, c, "fixture-one"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "native", "hi", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "native", api.EventMessageAgentCompleted)
	if _, err := c.Prompt(ctx, "fixture-one", "hi", "", ""); err == nil {
		t.Fatal("prompt must be rejected for a fixture session")
	}
	if _, err := c.Cancel(ctx, "fixture-one"); err == nil {
		t.Fatal("cancel must be rejected for a fixture session")
	}
	if d.supervisor.Len() != 2 {
		t.Fatalf("runtimes = %d, want 2 (one per harness)", d.supervisor.Len())
	}
	// Stopping the fixture session must not touch the Codex runtime.
	if _, err := c.StopSession(ctx, "fixture-one"); err != nil {
		t.Fatal(err)
	}
	if d.supervisor.Len() != 1 {
		t.Fatalf("runtimes = %d, want 1 after fixture stop", d.supervisor.Len())
	}
	if _, err := c.Prompt(ctx, "native", "still here", "", ""); err != nil {
		t.Fatalf("codex session broken by fixture stop: %v", err)
	}
	waitDurableTypes(t, c, "native", api.EventMessageAgentCompleted)
}

// TestCodexNativeServerNotExposed: the app-server is private to relayd.
// Driving a real turn must not open any new listening socket, the session
// DTO must carry no harness transport field, and there is no API route
// that proxies the native server.
func TestCodexNativeServerNotExposed(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "happy")
	_, p, c, _ := startInProcess(t, codexOptions)

	before := descendantListeningPorts(t)
	serveCodex(t, c, "private")
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "private", "hi", "", ""); err != nil {
		t.Fatal(err)
	}
	waitDurableTypes(t, c, "private", api.EventMessageAgentCompleted)
	after := descendantListeningPorts(t)
	for port := range after {
		if !before[port] {
			t.Fatalf("a spawned child opened a listening socket on port %d", port)
		}
	}

	// The raw session DTO carries no harness transport or credential.
	raw, err := os.ReadFile(p.DaemonDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	var desc api.Descriptor
	if err := json.Unmarshal(raw, &desc); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, c.Endpoint()+"/v1/sessions/private", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+desc.BearerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{"port", "endpoint", "socket", "listen", "stdio"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("session DTO leaks %q: %s", forbidden, body)
		}
	}
	// Token accounting is legitimate session data; a credential is not.
	for _, forbidden := range []string{`"token":`, "bearertoken", "credential", "password", "secret"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("session DTO leaks %q: %s", forbidden, body)
		}
	}
	// No route proxies the native server.
	for _, path := range []string{"/v1/sessions/private/app-server", "/v1/sessions/private/native"} {
		req, err := http.NewRequest(http.MethodGet, c.Endpoint()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+desc.BearerToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", path, resp.StatusCode)
		}
	}
}
