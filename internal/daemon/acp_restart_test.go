package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// TestACPSessionDTONeverLeaksNativeServer: the session projection carries the
// logical session and its native identity, never the daemon's private runtime
// details.
func TestACPSessionDTONeverLeaksNativeServer(t *testing.T) {
	state := newACPState(t, acp.FakeHappy)
	_, _, c, _ := startInProcess(t, state.options)
	info := createACP(t, c, session.HarnessDevin, "leak", t.TempDir())
	promptACP(t, c, info.Key, "hello")
	waitDurableTypes(t, c, info.Key, api.EventMessageAgentCompleted)

	ctx, cancel := tctx(t)
	list, err := c.Sessions(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"port", "endpoint", "socket", "listen", "stdio", "stderr"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("session list leaks %q: %s", forbidden, raw)
		}
	}
	for _, forbidden := range []string{`"token":`, "bearertoken", "credential", "password", "secret"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("session list leaks %q: %s", forbidden, raw)
		}
	}
}

// TestDaemonRestartRestoresACPSessionsColdAndResumesExactly: a restart reloads
// every ACP session COLD with its durable identity, and the next prompt resumes
// the exact native session.
func TestDaemonRestartRestoresACPSessionsColdAndResumesExactly(t *testing.T) {
	state := newACPState(t, acp.FakeHappy)
	d, p, c, served := startInProcess(t, state.options)
	info := createACP(t, c, session.HarnessOpenCode, "restart", t.TempDir())
	first := promptACP(t, c, info.Key, "one")
	waitDurableTypes(t, c, info.Key, api.EventMessageAgentCompleted)
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1", n)
	}

	d.Shutdown()
	select {
	case <-served:
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not shut down")
	}

	d2opts := Options{SelfExe: testBinary()}
	state.options(&d2opts)
	d2, err := Start(p, d2opts)
	if err != nil {
		t.Fatal(err)
	}
	served2 := make(chan error, 1)
	go func() { served2 <- d2.Serve() }()
	// Shutdown only initiates teardown; wait for Serve to finish draining
	// (runtime stops, watchers, adapter readers) inside the test lifetime.
	t.Cleanup(func() {
		d2.Shutdown()
		select {
		case <-served2:
		case <-time.After(20 * time.Second):
			t.Error("second daemon did not shut down")
		}
	})
	c2 := dialClient(t, p)

	// Restored COLD: no process, identity intact.
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "initialize"); n != 1 {
		t.Fatalf("restart must not spawn a runtime (initialize = %d)", n)
	}
	ctx, cancel := tctx(t)
	restored, err := c2.Status(ctx, info.Key)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Session.NativeSessionID != first.NativeSessionID {
		t.Fatalf("restored nativeSessionId = %q, want %q", restored.Session.NativeSessionID, first.NativeSessionID)
	}
	if restored.Session.State != session.StateIdle {
		t.Fatalf("restored state = %q, want idle", restored.Session.State)
	}

	// The next prompt resumes the EXACT native session.
	second := promptACP(t, c2, info.Key, "two")
	waitDurableTypes(t, c2, info.Key, api.EventMessageAgentCompleted)
	if second.NativeSessionID != first.NativeSessionID {
		t.Fatalf("resumed %q, want the exact %q", second.NativeSessionID, first.NativeSessionID)
	}
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "session/load"); n != 1 {
		t.Fatalf("session/load calls = %d, want 1", n)
	}
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "session/new"); n != 1 {
		t.Fatalf("session/new calls = %d, want 1 (no substitution)", n)
	}
}
