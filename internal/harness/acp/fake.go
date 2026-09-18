package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FakeConfig selects one deterministic fake ACP agent personality. The fake
// speaks the same protocol as the real vendors and persists native sessions
// under StateDir, but performs no model call, no network access, and no
// quota use.
type FakeConfig struct {
	// Vendor is "opencode" or "devin": it selects the native persistence
	// semantics (OpenCode can reattach to a zero-turn session, Devin
	// cannot) and the advertised config-option surface.
	Vendor string
	// Mode is the scripted scenario (see the fake* constants).
	Mode string
	// StateDir is the isolated native store root. It must be a test-owned
	// temporary directory.
	StateDir string
}

// Fake scenario modes (the deterministic test seam).
const (
	// FakeHappy streams chunks and completes with usage.
	FakeHappy = "happy"
	// FakeThought interleaves hidden reasoning with assistant text.
	FakeThought = "thought"
	// FakeCancel streams one chunk and completes only after cancel.
	FakeCancel = "cancel"
	// FakePermission asks for approval before completing.
	FakePermission = "permission"
	// FakePermissionUnknown asks for approval with no recognizable allow
	// option (the client must fail closed).
	FakePermissionUnknown = "permission-unknown"
	// FakeConfigReject rejects an unadvertised config value natively.
	FakeConfigReject = "config-reject"
	// FakeConfigSlow blocks the native config RPC until the test releases it
	// (by creating <state>/release-config), so the sleep-blocker behaviour of
	// a live config mutation is deterministic and sleep-free.
	FakeConfigSlow = "config-slow"
	// FakeFailTurn answers every prompt with a non-success stop reason
	// without the runtime dying: the turn fails but the generation lives.
	FakeFailTurn = "fail-turn"
	// FakeNoLoad advertises loadSession: false.
	FakeNoLoad = "no-load"
	// FakeLoadError fails every session/load.
	FakeLoadError = "load-error"
	// FakeDieOnPrompt exits while accepting a prompt.
	FakeDieOnPrompt = "die-on-prompt"
	// FakeDieOnPermission exits while an approval request is in flight.
	FakeDieOnPermission = "die-on-permission"
	// FakeDieIdle exits after the first completed turn.
	FakeDieIdle = "die-idle"
	// FakeMalformed writes a non-JSON frame.
	FakeMalformed = "malformed"
	// FakeOversized writes a frame past the frame bound.
	FakeOversized = "oversized"
	// FakeUnknownRequest sends an agent→client request the client does not
	// implement.
	FakeUnknownRequest = "unknown-request"
	// FakeStubborn ignores the cooperative stop signal.
	FakeStubborn = "stubborn"
	// FakeChild spawns a child process (process-tree teardown evidence).
	FakeChild = "child"
)

// FakeVendors.
const (
	// FakeVendorOpenCode is the OpenCode personality.
	FakeVendorOpenCode = "opencode"
	// FakeVendorDevin is the Devin personality.
	FakeVendorDevin = "devin"
)

// FakeEnvironment variables (hidden CLI seam, never part of the public
// contract).
const (
	// EnvFakeACPState is the native store root.
	EnvFakeACPState = "FAKE_ACP_STATE"
	// EnvFakeACPMode is the scripted scenario.
	EnvFakeACPMode = "FAKE_ACP_MODE"
	// EnvFakeACPVendor is the personality.
	EnvFakeACPVendor = "FAKE_ACP_VENDOR"
)

// fakeAgent is one fake ACP agent process.
type fakeAgent struct {
	cfg   FakeConfig
	conn  *Conn
	store *fakeStore
	// raw is a direct stdout writer used only for fault injection
	// (malformed/oversized frames), which must not go through framing.
	raw io.Writer
	// events records scenario observations for deterministic assertions.
	events *fakeEvents

	mu       sync.Mutex
	cancels  map[string]chan struct{}
	turnSeq  int
	stopping chan struct{}
}

// RunFakeAgent runs one fake ACP agent until its transport ends and returns
// the process exit code.
func RunFakeAgent(cfg FakeConfig, stdin io.Reader, stdout io.Writer) int {
	if cfg.Mode == "" {
		cfg.Mode = FakeHappy
	}
	store, err := newFakeStore(cfg.StateDir, cfg.Vendor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-acp: %v\n", err)
		return 2
	}
	conn := NewConn(stdout, stdin)
	agent := &fakeAgent{
		cfg:      cfg,
		conn:     conn,
		store:    store,
		raw:      stdout,
		events:   newFakeEvents(store.root),
		cancels:  map[string]chan struct{}{},
		stopping: make(chan struct{}),
	}
	agent.registerHandlers()
	if cfg.Mode == FakeChild {
		agent.spawnChild()
	}
	if cfg.Mode == FakeStubborn {
		agent.ignoreStop()
	}
	agent.startIdleExit()
	_ = conn.Start()
	return 0
}

// registerHandlers wires the protocol surface.
func (a *fakeAgent) registerHandlers() {
	a.conn.SetNotificationHandler(a.onNotification)
	a.conn.SetHandler(MethodInitialize, a.onInitialize)
	a.conn.SetHandler(MethodSessionNew, a.onSessionNew)
	a.conn.SetHandler(MethodSessionLoad, a.onSessionLoad)
	a.conn.SetHandler(MethodSessionPrompt, a.onSessionPrompt)
	a.conn.SetHandler(MethodSessionSetConfigOption, a.onSetConfigOption)
	a.conn.SetHandler(MethodSessionSetMode, a.onSetMode)
	a.conn.SetHandler(MethodSessionList, a.onSessionList)
}

func (a *fakeAgent) onNotification(method string, params json.RawMessage) {
	if method != MethodSessionCancel {
		return
	}
	var p CancelParams
	if json.Unmarshal(params, &p) != nil {
		return
	}
	a.mu.Lock()
	ch := a.cancels[p.SessionID]
	a.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	a.events.record("cancel", p.SessionID)
}

// cancelSignal returns (creating if needed) the cancel channel for a session.
func (a *fakeAgent) cancelSignal(sessionID string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.cancels[sessionID]
	if !ok {
		ch = make(chan struct{})
		a.cancels[sessionID] = ch
	}
	return ch
}

func (a *fakeAgent) onInitialize(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad initialize params: %w", err)
	}
	a.events.record("initialize", p.ClientInfo.Name)
	loadSession := a.cfg.Mode != FakeNoLoad
	res := InitializeResult{
		AgentCapabilities: AgentCapabilities{
			LoadSession: loadSession,
			PromptCapabilities: PromptCapabilities{
				Image: true,
			},
			SessionCapabilities: SessionCapabilities{
				Close: json.RawMessage("{}"),
				List:  json.RawMessage("{}"),
			},
		},
		AgentInfo: &ClientInfo{
			Name:    a.cfg.Vendor,
			Version: "fake-1",
		},
		ProtocolVersion: Version,
	}
	return res, nil
}

func (a *fakeAgent) onSessionNew(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p NewSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad session/new params: %w", err)
	}
	rec, err := a.store.create(p.Cwd)
	if err != nil {
		return nil, err
	}
	a.events.record("session/new", rec.ID)
	return NewSessionResult{
		ConfigOptions: a.configOptions(rec),
		SessionID:     rec.ID,
	}, nil
}

func (a *fakeAgent) onSessionLoad(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p LoadSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad session/load params: %w", err)
	}
	if a.cfg.Mode == FakeLoadError {
		return nil, fmt.Errorf("session %s cannot be loaded", p.SessionID)
	}
	rec, ok := a.store.load(p.SessionID)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", p.SessionID)
	}
	// The verified vendor difference: Devin cannot safely reattach to a
	// native session with no completed turn.
	if a.store.requiresTurn() && rec.Turns == 0 {
		return nil, fmt.Errorf("session %s has no resumable turn", p.SessionID)
	}
	a.events.record("session/load", rec.ID)
	return LoadSessionResult{
		ConfigOptions: a.configOptions(rec),
		SessionID:     rec.ID,
	}, nil
}

func (a *fakeAgent) onSessionList(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p ListSessionsParams
	_ = json.Unmarshal(params, &p)
	out := ListSessionsResult{Sessions: []SessionInfo{}}
	for _, rec := range a.store.list() {
		out.Sessions = append(out.Sessions, SessionInfo{
			Cwd:       rec.Cwd,
			SessionID: rec.ID,
			Title:     "fake " + rec.ID,
		})
	}
	return out, nil
}

func (a *fakeAgent) onSetMode(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p SetModeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad session/set_mode params: %w", err)
	}
	rec, ok := a.store.load(p.SessionID)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", p.SessionID)
	}
	if !a.modeAvailable(p.ModeID) {
		return nil, fmt.Errorf("mode not found: %s", p.ModeID)
	}
	rec.Mode = p.ModeID
	if err := a.store.save(rec); err != nil {
		return nil, err
	}
	a.events.record("session/set_mode", p.ModeID)
	return struct{}{}, nil
}

func (a *fakeAgent) onSetConfigOption(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p SetConfigOptionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("bad session/set_config_option params: %w", err)
	}
	if a.cfg.Mode == FakeConfigSlow {
		// Block the native mutation until the test releases it.
		a.events.record("config-blocked", p.ConfigID)
		release := filepath.Join(a.store.root, "release-config")
		for i := 0; i < 4000; i++ {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		a.events.record("config-released", p.ConfigID)
	}
	rec, ok := a.store.load(p.SessionID)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", p.SessionID)
	}
	value, _ := p.Value.(string)
	options := a.configOptions(rec)
	opt, found := FindOption(options, "", p.ConfigID)
	if !found {
		return nil, fmt.Errorf("unknown config option: %s", p.ConfigID)
	}
	if opt.Type == TypeSelect && !optionHasValue(opt, value) {
		return nil, fmt.Errorf("model not found: %s", value)
	}
	switch opt.Category {
	case CategoryModel:
		rec.Model = value
	case CategoryMode:
		rec.Mode = value
	}
	if err := a.store.save(rec); err != nil {
		return nil, err
	}
	a.events.record("set_config_option", p.ConfigID+"="+value)
	return SetConfigOptionResult{ConfigOptions: a.configOptions(rec)}, nil
}

// optionHasValue reports whether a select option advertises a value.
func optionHasValue(opt ConfigOption, value string) bool {
	for _, v := range opt.SelectOptions() {
		if v.Value == value {
			return true
		}
	}
	return false
}

// idleExit terminates the process after the first completed turn (the
// observer/sleep scenario).
func (a *fakeAgent) startIdleExit() {
	if a.cfg.Mode != FakeDieIdle {
		return
	}
	go func() {
		<-a.events.firstTurn
		os.Exit(0)
	}()
}
