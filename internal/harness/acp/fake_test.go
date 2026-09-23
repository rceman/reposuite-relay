package acp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// jsonRaw and decodeJSON keep the test handlers readable.
type jsonRaw = json.RawMessage

func decodeJSON(raw json.RawMessage, v any) error { return json.Unmarshal(raw, v) }

// fakeHarness is one in-process fake ACP agent plus a typed client.
type fakeHarness struct {
	client   *Client
	conn     *Conn
	stateDir string
	exit     chan int
}

// startFake runs the fake agent in-process over pipes (the same protocol path
// as a spawned process, without a process).
func startFake(t *testing.T, vendor, mode string) *fakeHarness {
	t.Helper()
	stateDir := t.TempDir()
	sr, cw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan int, 1)
	go func() {
		exit <- RunFakeAgent(FakeConfig{
			Vendor:   vendor,
			Mode:     mode,
			StateDir: stateDir,
		}, sr, sw)
	}()
	conn := NewConn(cw, cr)
	t.Cleanup(func() {
		conn.Close(ErrConnClosed)
		cw.Close()
		cr.Close()
		sr.Close()
		sw.Close()
	})
	go conn.Start()
	h := &fakeHarness{
		client:   NewClient(conn),
		conn:     conn,
		stateDir: stateDir,
		exit:     exit,
	}
	return h
}

func (h *fakeHarness) init(t *testing.T) InitializeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.client.Initialize(ctx, ClientInfo{
		Name:    "relay",
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *fakeHarness) newSession(t *testing.T, cwd string) NewSessionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.client.SessionNew(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *fakeHarness) prompt(t *testing.T, sessionID, text string) PromptResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.client.Prompt(ctx, PromptParams{
		Prompt:    []ContentBlock{TextBlock(text)},
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestFakeInitializeCapabilities: the handshake reports real capabilities,
// including the loadSession capability Relay depends on.
func TestFakeInitializeCapabilities(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeHappy)
	res := h.init(t)
	if !res.AgentCapabilities.LoadSession {
		t.Fatal("fake must advertise loadSession")
	}
	if res.ProtocolVersion != Version {
		t.Fatalf("protocolVersion = %d", res.ProtocolVersion)
	}
	if res.AgentInfo == nil || res.AgentInfo.Name != FakeVendorOpenCode {
		t.Fatalf("agentInfo = %+v", res.AgentInfo)
	}
}

// TestFakeNoLoadCapability: a runtime that does not advertise loadSession is
// observable to the adapter, which must fail closed.
func TestFakeNoLoadCapability(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeNoLoad)
	if h.init(t).AgentCapabilities.LoadSession {
		t.Fatal("no-load fake must not advertise loadSession")
	}
}

// TestFakeHappyTurnStreamsAndCompletes: updates stream, then the prompt
// response carries the terminal stop reason and usage.
func TestFakeHappyTurnStreamsAndCompletes(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeHappy)
	h.init(t)
	updates := make(chan Update, 16)
	h.conn.SetNotificationHandler(func(method string, params jsonRaw) {
		if method != MethodSessionUpdate {
			return
		}
		n, err := DecodeUpdate(params)
		if err == nil {
			updates <- n.Update
		}
	})
	sess := h.newSession(t, t.TempDir())
	res := h.prompt(t, sess.SessionID, "hello")
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}
	if res.Usage == nil || res.Usage.TotalTokens == nil || *res.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	var text strings.Builder
	usageSeen := false
	for {
		select {
		case u := <-updates:
			switch u.SessionUpdate {
			case UpdateAgentMessageChunk:
				text.WriteString(u.Text())
			case UpdateUsage:
				usageSeen = true
			}
			continue
		default:
		}
		break
	}
	if text.String() != "echo: hello" {
		t.Fatalf("streamed text = %q", text.String())
	}
	if !usageSeen {
		t.Fatal("no usage_update notification")
	}
}

// TestFakeThoughtChunksAreSeparate: hidden reasoning arrives as its own
// update kind, never as assistant text.
func TestFakeThoughtChunksAreSeparate(t *testing.T) {
	h := startFake(t, FakeVendorDevin, FakeThought)
	h.init(t)
	var assistant, thought strings.Builder
	h.conn.SetNotificationHandler(func(method string, params jsonRaw) {
		if method != MethodSessionUpdate {
			return
		}
		n, err := DecodeUpdate(params)
		if err != nil {
			return
		}
		switch n.Update.SessionUpdate {
		case UpdateAgentMessageChunk:
			assistant.WriteString(n.Update.Text())
		case UpdateAgentThoughtChunk:
			thought.WriteString(n.Update.Text())
		}
	})
	sess := h.newSession(t, t.TempDir())
	if res := h.prompt(t, sess.SessionID, "hello"); res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}
	if !strings.Contains(thought.String(), "hidden reasoning") {
		t.Fatalf("thought text = %q", thought.String())
	}
	if strings.Contains(assistant.String(), "hidden reasoning") {
		t.Fatalf("assistant text leaked reasoning: %q", assistant.String())
	}
}

// TestFakeCancelStopsTurn: cancel is a notification; the pending prompt call
// then resolves as cancelled.
func TestFakeCancelStopsTurn(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeCancel)
	h.init(t)
	sess := h.newSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call, err := h.client.StartPrompt(PromptParams{
		Prompt:    []ContentBlock{TextBlock("please wait")},
		SessionID: sess.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := h.client.Cancel(sess.SessionID); err != nil {
		t.Fatal(err)
	}
	var res PromptResult
	if err := call.Wait(ctx, &res); err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCancelled {
		t.Fatalf("stopReason = %q, want cancelled", res.StopReason)
	}
}

// TestFakePermissionRoundTrip: the agent asks for approval and the client's
// answer decides the turn.
func TestFakePermissionRoundTrip(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakePermission)
	h.init(t)
	h.conn.SetHandler(MethodRequestPermission, func(_ context.Context, _ int64, params jsonRaw) (any, error) {
		var p RequestPermissionParams
		if err := decodeJSON(params, &p); err != nil {
			return nil, err
		}
		opt, ok := SelectAllowOption(p.Options)
		if !ok {
			return Cancelled(), nil
		}
		return Selected(opt.OptionID), nil
	})
	sess := h.newSession(t, t.TempDir())
	res := h.prompt(t, sess.SessionID, "write")
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q", res.StopReason)
	}
	if got := CountFakeEvents(h.stateDir, "permission"); got != 1 {
		t.Fatalf("permission events = %d", got)
	}
	if !strings.Contains(strings.Join(ReadFakeEvents(h.stateDir), "\n"), "selected:always") {
		t.Fatalf("events = %v", ReadFakeEvents(h.stateDir))
	}
}

// TestFakePermissionUnknownFailsClosed: an unclassifiable approval request is
// answered "cancelled", never approved by position.
func TestFakePermissionUnknownFailsClosed(t *testing.T) {
	h := startFake(t, FakeVendorDevin, FakePermissionUnknown)
	h.init(t)
	h.conn.SetHandler(MethodRequestPermission, func(_ context.Context, _ int64, params jsonRaw) (any, error) {
		var p RequestPermissionParams
		if err := decodeJSON(params, &p); err != nil {
			return nil, err
		}
		if opt, ok := SelectAllowOption(p.Options); ok {
			return Selected(opt.OptionID), nil
		}
		return Cancelled(), nil
	})
	sess := h.newSession(t, t.TempDir())
	h.prompt(t, sess.SessionID, "write")
	if !strings.Contains(strings.Join(ReadFakeEvents(h.stateDir), "\n"), "cancelled:") {
		t.Fatalf("events = %v", ReadFakeEvents(h.stateDir))
	}
}

// TestFakeConfigRejectsUnadvertisedValue: a native rejection is a protocol
// error, so the caller cannot record a change that did not happen.
func TestFakeConfigRejectsUnadvertisedValue(t *testing.T) {
	h := startFake(t, FakeVendorOpenCode, FakeHappy)
	h.init(t)
	sess := h.newSession(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	params := StringValue("fake/model-zzz")
	params.ConfigID = "model"
	params.SessionID = sess.SessionID
	if _, err := h.client.SetConfigOption(ctx, params); err == nil {
		t.Fatal("unadvertised model must be rejected")
	}
	params.Value = "fake/model-b"
	res, err := h.client.SetConfigOption(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	opt, ok := FindOption(res.ConfigOptions, CategoryModel, "model")
	if !ok {
		t.Fatalf("no model option in %+v", res.ConfigOptions)
	}
	if v, _ := opt.CurrentString(); v != "fake/model-b" {
		t.Fatalf("currentValue = %q", v)
	}
}

// TestFakeVendorPersistenceDifference: OpenCode reattaches to a zero-turn
// session; Devin refuses one until a turn completed.
func TestFakeVendorPersistenceDifference(t *testing.T) {
	for _, tc := range []struct {
		vendor       string
		zeroTurnLoad bool
	}{
		{vendor: FakeVendorOpenCode, zeroTurnLoad: true},
		{vendor: FakeVendorDevin, zeroTurnLoad: false},
	} {
		h := startFake(t, tc.vendor, FakeHappy)
		h.init(t)
		cwd := t.TempDir()
		sess := h.newSession(t, cwd)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := h.client.SessionLoad(ctx, cwd, sess.SessionID)
		cancel()
		if tc.zeroTurnLoad && err != nil {
			t.Fatalf("%s zero-turn load failed: %v", tc.vendor, err)
		}
		if !tc.zeroTurnLoad && err == nil {
			t.Fatalf("%s zero-turn load must fail", tc.vendor)
		}
		h.prompt(t, sess.SessionID, "materialize")
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		res, err := h.client.SessionLoad(ctx2, cwd, sess.SessionID)
		if err != nil {
			t.Fatalf("%s post-turn load failed: %v", tc.vendor, err)
		}
		if res.SessionID != sess.SessionID {
			t.Fatalf("loaded %q, want the exact %q", res.SessionID, sess.SessionID)
		}
	}
}
