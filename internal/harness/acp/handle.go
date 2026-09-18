package acp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Stable handle errors. Adapters map them onto the canonical harness errors.
var (
	// ErrTurnInFlight means the session already has an in-flight turn.
	ErrTurnInFlight = errors.New("acp turn already in flight")
	// ErrNoTurn means there is no in-flight turn to interrupt.
	ErrNoTurn = errors.New("acp no turn in flight")
)

// Session is one ACP-level native session on a runtime: the protocol
// mechanics of a session (prompt/turn lifecycle, streaming accumulation,
// cancel, config surface, capabilities) with no Relay policy attached.
//
// Relay policy — durable identity, generation, materialization rules,
// canonical events — belongs to the vendor adapter, which owns this handle.
type Session struct {
	srv    *Server
	client *Client
	id     string
	cwd    string

	mu       sync.Mutex
	turn     *Turn
	turnSeq  int
	options  []ConfigOption
	modes    *ModeState
	models   *ModelState
	lastText string
}

// Turn is one in-flight ACP turn. ACP has no turn identifier, so Relay
// generates one; the native message id is reported when the runtime supplies
// it.
type Turn struct {
	// ID is the Relay-generated turn identifier.
	ID string

	done chan struct{}

	// terminal is the terminal-ownership claim: exactly one code path —
	// the normal prompt response or a runtime-death Fail — may settle the
	// turn. The loser is discarded, so a turn can never produce two
	// terminal outcomes under a runtime-death race.
	terminal atomic.Bool

	mu         sync.Mutex
	stopReason string
	usage      *Usage
	text       strings.Builder
	messageID  string
	err        error
}

// Done closes when the turn reached its terminal state.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Result returns the terminal turn outcome. It blocks until the turn ends.
func (t *Turn) Result() (stopReason, text, messageID string, usage *Usage, err error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopReason, t.text.String(), t.messageID, t.usage, t.err
}

// settle records the terminal outcome and unblocks Result exactly once:
// whichever path runs first owns the result; the loser is discarded.
func (t *Turn) settle(stopReason string, usage *Usage, err error) bool {
	if !t.terminal.CompareAndSwap(false, true) {
		return false
	}
	t.mu.Lock()
	t.stopReason = stopReason
	t.usage = usage
	t.err = err
	t.mu.Unlock()
	close(t.done)
	return true
}

// Fail settles a turn whose prompt response can never arrive — the runtime
// generation died and took the connection with it. Exactly once with
// settle: a response that still arrives on a dying connection loses.
func (t *Turn) Fail(err error) {
	t.settle("", nil, err)
}

// Text returns the assistant text accumulated so far (live view).
func (t *Turn) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.String()
}

// appendText accumulates streamed assistant text.
func (t *Turn) appendText(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text.WriteString(s)
}

// OpenSession creates a new native session on the runtime.
func OpenSession(ctx context.Context, srv *Server, cwd string) (*Session, error) {
	res, err := srv.client.SessionNew(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if !ValidSessionID(res.SessionID) {
		return nil, fmt.Errorf("session/new returned unusable session id %q", res.SessionID)
	}
	h := &Session{
		srv:     srv,
		client:  srv.client,
		id:      res.SessionID,
		cwd:     cwd,
		options: res.ConfigOptions,
		modes:   res.Modes,
		models:  res.Models,
	}
	srv.RegisterSession(h)
	return h, nil
}

// AttachSession reattaches to an EXACT native session. It never substitutes a
// different session: a failure, or a runtime that reports another identity,
// is a hard failure the caller surfaces as NATIVE_SESSION_LOST.
func AttachSession(ctx context.Context, srv *Server, cwd, sessionID string) (*Session, error) {
	if !ValidSessionID(sessionID) {
		return nil, fmt.Errorf("refusing to load malformed native session id %q", sessionID)
	}
	res, err := srv.client.SessionLoad(ctx, cwd, sessionID)
	if err != nil {
		return nil, err
	}
	h := &Session{
		srv:     srv,
		client:  srv.client,
		id:      res.SessionID,
		cwd:     cwd,
		options: res.ConfigOptions,
		modes:   res.Modes,
		models:  res.Models,
	}
	srv.RegisterSession(h)
	return h, nil
}

// ID is the exact native session identity.
func (h *Session) ID() string { return h.id }

// Cwd is the working directory the native session was opened in.
func (h *Session) Cwd() string { return h.cwd }

// Close detaches the handle from the runtime routing table. It never deletes
// native session history.
func (h *Session) Close() { h.srv.UnregisterSession(h.id) }

// Options returns the last-known advertised config options.
func (h *Session) Options() []ConfigOption {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ConfigOption(nil), h.options...)
}

// Modes returns the last-known advertised mode surface.
func (h *Session) Modes() *ModeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.modes
}

// Models returns the last-known advertised model surface.
func (h *Session) Models() *ModelState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.models
}

// CurrentMode returns the last-known native mode id.
func (h *Session) CurrentMode() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.modes == nil {
		return ""
	}
	return h.modes.CurrentModeID
}

// CurrentModel returns the last-known native model id (empty when the
// runtime does not advertise one).
func (h *Session) CurrentModel() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.models != nil && h.models.CurrentModelID != "" {
		return h.models.CurrentModelID
	}
	if opt, ok := FindOption(h.options, CategoryModel, "model"); ok {
		if v, ok := opt.CurrentString(); ok {
			return v
		}
	}
	return ""
}

// StartPrompt submits one prompt and returns the in-flight turn. The turn
// ends when the runtime answers the prompt request (or the transport dies).
func (h *Session) StartPrompt(text string) (*Turn, error) {
	h.mu.Lock()
	if h.turn != nil {
		id := h.turn.ID
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: turn %s", ErrTurnInFlight, id)
	}
	h.turnSeq++
	turn := &Turn{
		ID:   "acp-" + strconv.Itoa(h.turnSeq),
		done: make(chan struct{}),
	}
	h.turn = turn
	h.mu.Unlock()

	call, err := h.client.StartPrompt(PromptParams{
		Prompt:    []ContentBlock{TextBlock(text)},
		SessionID: h.id,
	})
	if err != nil {
		h.finish(turn, "", nil, err)
		return nil, err
	}
	go func() {
		// The turn's own lifetime is unbounded by the control plane: it ends
		// when the runtime answers, when cancel resolves it, or when the
		// transport dies (which fails the pending call).
		var res PromptResult
		waitErr := call.Wait(context.Background(), &res)
		h.finish(turn, res.StopReason, res.Usage, waitErr)
	}()
	return turn, nil
}

// finish records the terminal turn state. The terminal is owned by whichever
// path settles first: a runtime-death Fail that already won discards this
// native outcome — the death is the truth.
func (h *Session) finish(turn *Turn, stopReason string, usage *Usage, err error) {
	turn.settle(stopReason, usage, err)

	h.mu.Lock()
	if h.turn == turn {
		h.turn = nil
	}
	h.lastText = turn.Text()
	h.mu.Unlock()
}

// Turn returns the in-flight turn, or nil.
func (h *Session) Turn() *Turn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turn
}

// LastText returns the assistant text of the most recent turn.
func (h *Session) LastText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastText
}

// Cancel interrupts the in-flight turn. ACP spells cancel as a notification,
// so success means the interrupt was delivered; the terminal state arrives
// through the pending prompt response.
func (h *Session) Cancel() error {
	if h.Turn() == nil {
		return ErrNoTurn
	}
	return h.client.Cancel(h.id)
}

// applyUpdate folds one streaming update into the session's state.
func (h *Session) applyUpdate(u Update) {
	switch u.SessionUpdate {
	case UpdateAgentMessageChunk:
		if turn := h.Turn(); turn != nil {
			turn.appendText(u.Text())
		}
	case UpdateConfigOption:
		h.mu.Lock()
		h.options = u.ConfigOptions
		h.mu.Unlock()
	case UpdateCurrentMode:
		h.mu.Lock()
		if h.modes != nil {
			h.modes.CurrentModeID = u.CurrentModeID
		}
		h.mu.Unlock()
	}
}

// SetConfigValue applies one advertised config option value. The runtime's
// own answer decides success: Relay records a change only after the runtime
// accepted it.
func (h *Session) SetConfigValue(ctx context.Context, configID string, value any, boolean bool) ([]ConfigOption, error) {
	params := SetConfigOptionParams{
		ConfigID:  configID,
		SessionID: h.id,
		Value:     value,
	}
	if boolean {
		params.Type = TypeBoolean
	}
	res, err := h.client.SetConfigOption(ctx, params)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if len(res.ConfigOptions) > 0 {
		h.options = res.ConfigOptions
	}
	if opt, ok := FindOption(h.options, CategoryModel, "model"); ok {
		if v, ok := opt.CurrentString(); ok && h.models != nil {
			h.models.CurrentModelID = v
		}
	}
	h.mu.Unlock()
	return res.ConfigOptions, nil
}

// SetMode applies one advertised mode id.
func (h *Session) SetMode(ctx context.Context, modeID string) error {
	if err := h.client.SetMode(ctx, h.id, modeID); err != nil {
		return err
	}
	h.mu.Lock()
	if h.modes != nil {
		h.modes.CurrentModeID = modeID
	}
	h.mu.Unlock()
	return nil
}

// ValidSessionID reports whether a native ACP session identity is usable:
// non-empty, bounded, and free of path/whitespace characters that would make
// it ambiguous.
func ValidSessionID(id string) bool {
	if id == "" || len(id) > MaxSessionIDBytes {
		return false
	}
	if strings.TrimSpace(id) != id {
		return false
	}
	return !strings.ContainsAny(id, "/\\\x00\n")
}
