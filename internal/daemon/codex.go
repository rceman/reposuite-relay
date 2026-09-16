package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/codex"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// harnessCallTimeout bounds one native control call (prompt submission,
// cancel, answer). It is a control-plane bound, not a turn bound: a turn
// completes through notifications and is never bounded here.
const harnessCallTimeout = 30 * time.Second

// maxPromptBytes bounds one prompt text.
const maxPromptBytes = 256 << 10

// handleCreateCodex creates a durable Codex session. Creation is a
// runtime-free (COLD) operation: no app-server is spawned until the first
// prompt. The adapter command is resolved up front so a machine without
// the harness fails closed at create instead of at first use.
func (d *Daemon) handleCreateCodex(w http.ResponseWriter, r *http.Request, _ string) {
	var req api.CodexRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if !session.ValidKey(req.Key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+req.Key)
		return
	}
	if req.Cwd == "" || !strings.HasPrefix(req.Cwd, "/") {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "serve requires an absolute client cwd")
		return
	}
	if _, err := d.opts.CodexCommand(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable,
			"codex harness unavailable: "+err.Error())
		return
	}
	if !d.registry.Reserve(req.Key) {
		writeErr(w, http.StatusConflict, api.ErrSessionExists, "session "+req.Key+" already exists")
		return
	}
	fail := func(status int, code, msg string) {
		d.registry.Cancel(req.Key)
		writeErr(w, status, code, msg)
	}
	sessionID, err := d.opts.RandSessionID()
	if err != nil {
		fail(http.StatusInternalServerError, api.ErrInternal, "session id: "+err.Error())
		return
	}
	now := time.Now()
	rs := &session.RelaySession{
		ID:         sessionID,
		Key:        req.Key,
		Harness:    session.HarnessCodex,
		Cwd:        req.Cwd,
		State:      session.StateIdle,
		Generation: 1,
		Model:      req.Model,
		Mode:       req.Mode,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := d.store.Create(rs); err != nil {
		var ue *store.UncertainError
		if errors.As(err, &ue) {
			go d.initiate()
			fail(http.StatusInternalServerError, api.ErrInternal,
				"store commit uncertain; daemon halting: "+err.Error())
			return
		}
		fail(http.StatusInternalServerError, api.ErrInternal, "persist session: "+err.Error())
		return
	}
	m := &session.Managed{Session: rs}
	if !d.registry.Commit(req.Key, m) {
		_ = d.store.Delete(rs.ID)
		d.registry.Cancel(req.Key)
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "lost key reservation")
		return
	}
	if _, err := d.broker.Ensure(m); err != nil {
		fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
		go d.initiate()
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "event state: "+err.Error())
		return
	}
	d.adapter.Track(m)
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}

// handlePrompt accepts a user prompt and submits it as a native turn.
// 202 Accepted means the native harness accepted the turn; the canonical
// durable message.user event was published first (see codex.Adapter).
func (d *Daemon) handlePrompt(w http.ResponseWriter, r *http.Request, key string) {
	m, ok := d.codexSession(w, key)
	if !ok {
		return
	}
	var req api.PromptRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "prompt text is required")
		return
	}
	if len(req.Text) > maxPromptBytes {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "prompt text exceeds the bound")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), harnessCallTimeout)
	defer cancel()
	res, err := d.adapter.Prompt(ctx, m, req.Text, req.Model, req.Effort)
	if err != nil {
		writeCodexErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, api.PromptResponse{
		Daemon: d.info(), Key: key, TurnID: res.TurnID,
		NativeThreadID: res.NativeThreadID, RuntimeID: res.RuntimeID,
	})
}

// handleCancel interrupts the in-flight native turn.
func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request, key string) {
	m, ok := d.codexSession(w, key)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), harnessCallTimeout)
	defer cancel()
	if err := d.adapter.Cancel(ctx, m); err != nil {
		writeCodexErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.CancelResponse{Daemon: d.info(), Key: key})
}

// handleInput answers a native requested-input request by exact Relay
// input ID.
func (d *Daemon) handleInput(w http.ResponseWriter, r *http.Request, key string) {
	m, ok := d.codexSession(w, key)
	if !ok {
		return
	}
	var req api.InputRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.InputID == "" {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "inputId is required")
		return
	}
	sel := make([]codex.AnswerSelection, 0, len(req.Answers))
	for _, a := range req.Answers {
		sel = append(sel, codex.AnswerSelection{QuestionID: a.QuestionID, Answers: a.Answers})
	}
	ctx, cancel := context.WithTimeout(r.Context(), harnessCallTimeout)
	defer cancel()
	if err := d.adapter.AnswerInput(ctx, m, req.InputID, sel); err != nil {
		writeCodexErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.InputResponse{Daemon: d.info(), Key: key, InputID: req.InputID})
}

// handleConfig accepts a model/mode change for a Codex session.
func (d *Daemon) handleConfig(w http.ResponseWriter, r *http.Request, key string) {
	m, ok := d.codexSession(w, key)
	if !ok {
		return
	}
	var req api.ConfigRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Model == nil && req.Mode == nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidConfig, "no configuration fields supplied")
		return
	}
	snap := m.Snapshot()
	cmd := codex.ConfigCommand{Model: snap.Model, Mode: snap.Mode}
	if req.Model != nil {
		cmd.Model = strings.TrimSpace(*req.Model)
	}
	if req.Mode != nil {
		cmd.Mode = strings.TrimSpace(*req.Mode)
	}
	ctx, cancel := context.WithTimeout(r.Context(), harnessCallTimeout)
	defer cancel()
	if err := d.adapter.ApplyConfig(ctx, m, cmd); err != nil {
		writeCodexErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}

// codexSession resolves a key to a live Codex session.
func (d *Daemon) codexSession(w http.ResponseWriter, key string) (*session.Managed, bool) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return nil, false
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return nil, false
	}
	if m.Snapshot().Harness != session.HarnessCodex {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest,
			"session "+key+" is not a codex session")
		return nil, false
	}
	return m, true
}

// writeCodexErr maps adapter errors to stable wire codes. A busy session
// or a missing turn is a conflict; a lost native session or a dead
// runtime is a conflict too — the caller must re-establish context.
func writeCodexErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, codex.ErrBusy):
		writeErr(w, http.StatusConflict, api.ErrSessionBusy, err.Error())
	case errors.Is(err, codex.ErrNoActiveTurn):
		writeErr(w, http.StatusConflict, api.ErrNoActiveTurn, err.Error())
	case errors.Is(err, codex.ErrNativeSessionLost):
		writeErr(w, http.StatusConflict, api.ErrNativeSessionLost, err.Error())
	case errors.Is(err, codex.ErrNoSuchInput):
		writeErr(w, http.StatusNotFound, api.ErrUnknownInput, err.Error())
	case errors.Is(err, codex.ErrRuntimeUnavailable):
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, api.ErrRuntimeUnavailable, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
	}
}
