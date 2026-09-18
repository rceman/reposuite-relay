package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"net/http"
	"os"
	"strings"
	"time"
)

// creationSpec is one built-in creation route. Creation is always a
// runtime-free (COLD) durable operation: no harness process is spawned
// until the first prompt. Generation counts successful native runtime
// BINDINGS, so every native harness session starts at 0 — the first
// binding happens on the first prompt.
//
// There is deliberately no executable/argv field anywhere in this path.
type creationSpec struct {
	harness    string
	generation int
}

var creationSpecs = map[string]creationSpec{
	session.HarnessCodex:    {harness: session.HarnessCodex, generation: 0},
	session.HarnessOpenCode: {harness: session.HarnessOpenCode, generation: 0},
	session.HarnessDevin:    {harness: session.HarnessDevin, generation: 0},
}

// handleCreateHarness creates a durable session for one built-in harness.
// The adapter command is resolved up front so a machine without the
// harness fails closed at create instead of at first use.
func (d *Daemon) handleCreateHarness(w http.ResponseWriter, r *http.Request, name string) {
	spec, ok := creationSpecs[name]
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown harness "+name)
		return
	}
	entry, ok := d.entryFor(spec.harness)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable,
			"no adapter for harness "+spec.harness)
		return
	}
	var req api.CreateRequest
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
	if err := entry.available(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable,
			spec.harness+" harness unavailable: "+err.Error())
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
		Harness:    spec.harness,
		Cwd:        req.Cwd,
		State:      session.StateIdle,
		Generation: spec.generation,
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
	entry.adapter.Track(m)
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}

// handlePrompt accepts a user prompt and submits it as a native turn.
// 202 Accepted means the native harness accepted the turn; the canonical
// durable message.user event was published first.
func (d *Daemon) handlePrompt(w http.ResponseWriter, r *http.Request, key string) {
	m, adapter, ok := d.harnessSession(w, key)
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
	ctx, cancel := contextWithHarnessTimeout(r)
	defer cancel()
	res, err := adapter.Prompt(ctx, m, harness.PromptCommand{
		Text:   req.Text,
		Model:  req.Model,
		Effort: req.Effort,
	})
	if err != nil {
		writeAdapterErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, api.PromptResponse{
		Daemon: d.info(), Key: key, TurnID: res.TurnID,
		NativeSessionID: res.NativeSessionID, RuntimeID: res.RuntimeID,
	})
}

// handleCancel interrupts the in-flight native turn through the session's
// own adapter — each harness receives its native cancel mechanism.
func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request, key string) {
	m, adapter, ok := d.harnessSession(w, key)
	if !ok {
		return
	}
	ctx, cancel := contextWithHarnessTimeout(r)
	defer cancel()
	if err := adapter.Cancel(ctx, m); err != nil {
		writeAdapterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.CancelResponse{Daemon: d.info(), Key: key})
}

// handleInput answers a native requested-input request by exact Relay
// input ID. A harness without a verified elicitation path answers
// UNSUPPORTED_OPERATION, never INTERNAL.
func (d *Daemon) handleInput(w http.ResponseWriter, r *http.Request, key string) {
	m, adapter, ok := d.harnessSession(w, key)
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
	answers := make([]harness.InputAnswer, 0, len(req.Answers))
	for _, a := range req.Answers {
		answers = append(answers, harness.InputAnswer{QuestionID: a.QuestionID, Answers: a.Answers})
	}
	ctx, cancel := contextWithHarnessTimeout(r)
	defer cancel()
	if err := adapter.AnswerInput(ctx, m, req.InputID, answers); err != nil {
		writeAdapterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.InputResponse{Daemon: d.info(), Key: key, InputID: req.InputID})
}

// handleConfig accepts a model/mode change for a session through its own
// adapter. A capability mismatch is UNSUPPORTED_OPERATION.
func (d *Daemon) handleConfig(w http.ResponseWriter, r *http.Request, key string) {
	m, adapter, ok := d.harnessSession(w, key)
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
	cmd := harness.ConfigCommand{Model: snap.Model, Mode: snap.Mode}
	if req.Model != nil {
		cmd.Model = strings.TrimSpace(*req.Model)
	}
	if req.Mode != nil {
		cmd.Mode = strings.TrimSpace(*req.Mode)
	}
	ctx, cancel := contextWithHarnessTimeout(r)
	defer cancel()
	if err := adapter.ApplyConfig(ctx, m, cmd); err != nil {
		writeAdapterErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}

// contextWithHarnessTimeout bounds one native control call. It is a
// control-plane bound, not a turn bound: a turn completes through native
// notifications and is never bounded here.
func contextWithHarnessTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), harnessCallTimeout)
}

// harnessCallTimeout bounds one native control call (prompt submission,
// cancel, answer, config).
const harnessCallTimeout = 30 * time.Second

// maxPromptBytes bounds one prompt text.
const maxPromptBytes = 256 << 10
