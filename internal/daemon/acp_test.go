package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// acpVendor pairs one ACP harness with its fake-agent vendor and an isolated
// native state root, so cross-adapter isolation is observable from the outside.
type acpVendor struct {
	harness string
	vendor  string
	state   string
}

// acpTestState owns the fake native state roots for one test, so every daemon
// generation in that test speaks to the SAME native store (which is what makes
// exact resume observable across a restart).
type acpTestState struct {
	opencode acpVendor
	devin    acpVendor
	mode     string
}

func newACPState(t *testing.T, mode string) *acpTestState {
	t.Helper()
	base := t.TempDir()
	return &acpTestState{
		mode: mode,
		opencode: acpVendor{
			harness: session.HarnessOpenCode,
			vendor:  acp.FakeVendorOpenCode,
			state:   filepath.Join(base, "opencode"),
		},
		devin: acpVendor{
			harness: session.HarnessDevin,
			vendor:  acp.FakeVendorDevin,
			state:   filepath.Join(base, "devin"),
		},
	}
}

// options points both ACP adapters at the production binary's hidden
// deterministic fake-agent mode, each with its own native state root.
func (s *acpTestState) options(o *Options) {
	o.OpenCodeCommand = func() (acp.Command, error) {
		return acp.Command{
			Path: testBinary(),
			Args: []string{"__fake-acp"},
			Env:  fakeEnv(s.opencode, s.mode),
		}, nil
	}
	o.DevinCommand = func(model string) (acp.Command, error) {
		return acp.Command{
			Path: testBinary(),
			Args: []string{"__fake-acp"},
			Env:  fakeEnv(s.devin, s.mode),
		}, nil
	}
}

// dir returns a harness's fake native state root.
func (s *acpTestState) dir(harness string) string {
	switch harness {
	case session.HarnessOpenCode:
		return s.opencode.state
	case session.HarnessDevin:
		return s.devin.state
	}
	return ""
}

// fakeEnv builds the fake agent's environment.
func fakeEnv(v acpVendor, mode string) []string {
	return append(os.Environ(),
		acp.EnvFakeACPVendor+"="+v.vendor,
		acp.EnvFakeACPMode+"="+mode,
		acp.EnvFakeACPState+"="+v.state,
	)
}

// createACP creates one ACP session through the public API.
func createACP(t *testing.T, c *client.Client, harness, key, cwd string) api.SessionInfo {
	t.Helper()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.CreateSession(ctx, harness, key, cwd, "", "")
	if err != nil {
		t.Fatalf("create %s session: %v", harness, err)
	}
	return resp.Session
}

// promptACP prompts and returns the accepted native identity.
func promptACP(t *testing.T, c *client.Client, key, text string) api.PromptResponse {
	t.Helper()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.Prompt(ctx, key, text, "", "")
	if err != nil {
		t.Fatalf("prompt %s: %v", key, err)
	}
	return resp
}

// TestACPHarnessRoutingIsIsolated: the daemon routes each harness to its own
// adapter and its own native runtime; nothing crosses over.
func TestACPHarnessRoutingIsIsolated(t *testing.T) {
	state := newACPState(t, acp.FakeHappy)
	_, _, c, _ := startInProcess(t, state.options)
	devin := createACP(t, c, session.HarnessDevin, "iso-devin", t.TempDir())
	opencode := createACP(t, c, session.HarnessOpenCode, "iso-opencode", t.TempDir())

	// Both are COLD: creation starts no process at all.
	for _, v := range []string{session.HarnessDevin, session.HarnessOpenCode} {
		if n := acp.CountFakeEvents(state.dir(v), "initialize"); n != 0 {
			t.Fatalf("%s: creation must not start a runtime (initialize = %d)", v, n)
		}
	}

	devinPrompt := promptACP(t, c, devin.Key, "hello devin")
	opencodePrompt := promptACP(t, c, opencode.Key, "hello opencode")
	if devinPrompt.NativeSessionID == "" || opencodePrompt.NativeSessionID == "" {
		t.Fatalf("native identities missing: %+v %+v", devinPrompt, opencodePrompt)
	}
	waitDurableTypes(t, c, devin.Key, api.EventMessageAgentCompleted)
	waitDurableTypes(t, c, opencode.Key, api.EventMessageAgentCompleted)

	// One runtime each, and no session ever appears in the other's native store.
	for _, v := range []acpVendor{
		{harness: session.HarnessDevin, state: state.dir(session.HarnessDevin)},
		{harness: session.HarnessOpenCode, state: state.dir(session.HarnessOpenCode)},
	} {
		if n := acp.CountFakeEvents(v.state, "initialize"); n != 1 {
			t.Fatalf("%s: initialize calls = %d, want 1", v.harness, n)
		}
		if n := acp.CountFakeEvents(v.state, "session/new"); n != 1 {
			t.Fatalf("%s: session/new calls = %d, want 1", v.harness, n)
		}
	}
	devinIDs := acp.FakeSessionIDs(state.dir(session.HarnessDevin))
	opencodeIDs := acp.FakeSessionIDs(state.dir(session.HarnessOpenCode))
	if len(devinIDs) != 1 || len(opencodeIDs) != 1 {
		t.Fatalf("native stores = %v / %v", devinIDs, opencodeIDs)
	}
	if devinIDs[0] == opencodeIDs[0] {
		t.Fatal("the two adapters must not share a native identity")
	}
	if devinPrompt.NativeSessionID != devinIDs[0] {
		t.Fatalf("devin identity = %q, want %q", devinPrompt.NativeSessionID, devinIDs[0])
	}
	if opencodePrompt.NativeSessionID != opencodeIDs[0] {
		t.Fatalf("opencode identity = %q, want %q", opencodePrompt.NativeSessionID, opencodeIDs[0])
	}
}

// TestACPInputEndpointIsUnsupported: neither ACP harness exposes Relay's
// requested-input surface, and the API says so with a stable code.
func TestACPInputEndpointIsUnsupported(t *testing.T) {
	state := newACPState(t, acp.FakePermission)
	_, _, c, _ := startInProcess(t, state.options)
	for _, harness := range []string{session.HarnessDevin, session.HarnessOpenCode} {
		key := "input-" + harness
		createACP(t, c, harness, key, t.TempDir())
		promptACP(t, c, key, "write something")
		waitDurableTypes(t, c, key, api.EventMessageAgentCompleted)
		ctx, cancel := tctx(t)
		_, err := c.AnswerInput(ctx, key, "in-1", []api.InputAnswer{
			{QuestionID: "q1", Answers: []string{"yes"}},
		})
		cancel()
		wantAPIErr(t, err, api.ErrUnsupportedOperation)
	}
}

// TestObserverStreamDoesNotWakeRuntime: attaching an observer to a COLD
// session must not start a process.
func TestObserverStreamDoesNotWakeRuntime(t *testing.T) {
	state := newACPState(t, acp.FakeHappy)
	_, _, c, _ := startInProcess(t, state.options)
	info := createACP(t, c, session.HarnessOpenCode, "observer", t.TempDir())

	ctx, cancel := tctx(t)
	stream, err := c.Events(ctx, info.Key, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	// Give the stream a moment to attach, then prove nothing woke up.
	time.Sleep(200 * time.Millisecond)
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "initialize"); n != 0 {
		t.Fatalf("observing a COLD session woke a runtime (initialize = %d)", n)
	}
	// Status reads must not wake it either.
	statusCtx, statusCancel := tctx(t)
	if _, err := c.Status(statusCtx, info.Key); err != nil {
		t.Fatalf("status: %v", err)
	}
	statusCancel()
	if n := acp.CountFakeEvents(state.dir(session.HarnessOpenCode), "initialize"); n != 0 {
		t.Fatalf("status read woke a runtime (initialize = %d)", n)
	}

	// The observer still receives the canonical stream once work happens.
	promptACP(t, c, info.Key, "hello")
	got := readTypes(t, stream, 10*time.Second, api.EventMessageAgentCompleted)
	cancel()
	if !got[api.EventMessageAgentCompleted] {
		t.Fatalf("observer stream types = %v", got)
	}
}

// readTypes drains an event stream until a wanted type arrives.
func readTypes(t *testing.T, stream *client.EventStream, budget time.Duration, want string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		ev, err := stream.Next()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		seen[ev.Type] = true
		if ev.Type == want {
			return seen
		}
	}
	return seen
}
