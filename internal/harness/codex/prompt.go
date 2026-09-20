package codex

import (
	"context"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
)

// Prompt accepts a user prompt and submits it as a native turn:
//
//	message.user (durable, accepted by Relay) → runtime ensure/initialize
//	→ thread/start (first prompt) or thread/resume (after a rebind)
//	→ turn/start → in-flight turn state.
//
// A failure after acceptance publishes a durable turn.failed event, so
// the prompt is never erased from history.
func (a *Adapter) Prompt(ctx context.Context, m *session.Managed, cmd harness.PromptCommand) (harness.PromptResult, error) {
	text, model, effort := cmd.Text, cmd.Model, cmd.Effort
	st := a.state(m.Session.ID)
	if st == nil {
		return harness.PromptResult{}, fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	if st.current != nil {
		turn := st.current.turnID
		a.mu.Unlock()
		return harness.PromptResult{}, fmt.Errorf("%w: turn %s in flight", ErrBusy, turn)
	}
	a.mu.Unlock()

	// The turn is in flight from the moment Relay accepts it: activity is
	// a sleep blocker for the whole submission.
	a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityActive)

	if model == "" {
		model = m.Snapshot().Model
	}
	mode := m.Snapshot().Mode
	if err := a.publishDurable(m, api.EventMessageUser, api.MessageUserPayload{
		Text:   text,
		Model:  model,
		Effort: effort,
	}); err != nil {
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		return harness.PromptResult{}, err
	}

	fail := func(err error) (harness.PromptResult, error) {
		_ = a.publishDurable(m, api.EventTurnFailed, api.TurnEventPayload{Error: err.Error()})
		a.deps.Supervisor.SetActivity(m.Session.ID, runtime.ActivityIdle)
		_ = a.setSessionState(m, session.StateIdle)
		return harness.PromptResult{}, err
	}

	// Reuse the live thread when this exact runtime generation already
	// owns it; otherwise start a new thread or resume the exact recorded
	// native session. Validated BEFORE any runtime is spawned: a malformed
	// durable identity must not wake a process at all.
	a.mu.Lock()
	threadID := st.nativeID
	live := st.liveKey == RuntimeKey
	a.mu.Unlock()
	if live {
		if _, ok := a.deps.Supervisor.View(m.Session.ID); !ok {
			// The generation hosting this binding died while the death
			// path could not see it (the session was mid-bind): the
			// in-memory thread died with the process, so drop the stale
			// routing and re-bind exactly.
			a.mu.Lock()
			if st.liveKey == RuntimeKey {
				delete(a.threads, st.nativeID)
				st.liveKey = ""
				st.nativeID = ""
				if st.materialized {
					st.nativeID = m.Snapshot().NativeSessionID
				}
				threadID = st.nativeID
			}
			a.mu.Unlock()
			live = false
		}
	}
	resume := !live && threadID != ""
	if resume && !harness.ValidNativeID(threadID) {
		return fail(fmt.Errorf("%w: malformed native session id %q",
			ErrNativeSessionLost, threadID))
	}

	srv, err := a.ensureRuntime(ctx, m.Session.Cwd)
	if err != nil {
		return fail(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	approval := ApprovalNever
	var threadModel string
	if !live {
		if threadID == "" {
			res, err := srv.ThreadStart(callCtx, ThreadStartParams{
				Model:          model,
				Cwd:            m.Session.Cwd,
				ApprovalPolicy: approval,
				ServiceTier:    mode,
			})
			if err != nil {
				return fail(fmt.Errorf("thread/start: %w%s", err, stderrSuffix(srv)))
			}
			if res.Thread.ID == "" {
				return fail(errors.New("thread/start returned no thread id"))
			}
			threadID = res.Thread.ID
			threadModel = res.Model
		} else {
			res, err := srv.ThreadResume(callCtx, ThreadResumeParams{
				ThreadID:       threadID,
				Model:          model,
				Cwd:            m.Session.Cwd,
				ApprovalPolicy: approval,
				ServiceTier:    mode,
			})
			if err != nil {
				return fail(fmt.Errorf("%w: thread/resume %s: %v%s",
					ErrNativeSessionLost, threadID, err, stderrSuffix(srv)))
			}
			if res.Thread.ID != threadID {
				return fail(fmt.Errorf("%w: thread/resume returned %q, want %q",
					ErrNativeSessionLost, res.Thread.ID, threadID))
			}
			threadModel = res.Model
		}
		// Bind only after the thread exists in this generation. The
		// generation counts successful native runtime BINDINGS: this is a
		// fresh binding even while the native identity is still provisional
		// (Generation=1 with NativeSessionID="" is a valid state before the
		// first completed turn).
		if err := a.deps.Supervisor.Bind(RuntimeKey, m.Session.ID); err != nil {
			return fail(err)
		}
		// The generation bump is durable only when the commit succeeds; a
		// failed commit must not leave the supervisor binding behind, so it
		// is rolled back synchronously before the prompt fails.
		if err := a.deps.Materialize(m, harness.SessionUpdate{BumpGeneration: true}); err != nil {
			a.deps.Supervisor.Unbind(m.Session.ID)
			return fail(err)
		}
		// The durable commit stands, but the generation may have died
		// mid-transaction (the death path could not see the binding
		// because routing did not exist yet): no routing may claim a
		// dead generation and no start may be advertised for it.
		if _, ok := a.deps.Supervisor.View(m.Session.ID); !ok {
			return fail(fmt.Errorf("%w: runtime generation exited during bind",
				ErrRuntimeUnavailable))
		}
		a.mu.Lock()
		st.nativeID = threadID
		st.liveKey = RuntimeKey
		a.threads[threadID] = m.Session.ID
		firstTurn := !st.materialized
		st.turnSeq++
		st.current = &activeTurn{
			nativeThreadID: threadID,
			firstTurn:      firstTurn,
		}
		a.mu.Unlock()
		if err := a.publishDurable(m, api.EventHarnessStarted, api.HarnessStartedPayload{
			RuntimeID:       a.runtimeID(m.Session.ID),
			NativeSessionID: threadID,
			Model:           threadModel,
			Resumed:         resume,
		}); err != nil {
			return fail(err)
		}
	} else {
		a.mu.Lock()
		firstTurn := !st.materialized
		st.turnSeq++
		st.current = &activeTurn{
			nativeThreadID: threadID,
			firstTurn:      firstTurn,
		}
		a.mu.Unlock()
	}

	turnParams := TurnStartParams{
		ThreadID: threadID,
		Input:    []UserInput{TextInput(text)},
		Model:    model,
		Effort:   effort,
	}
	if mode := m.Snapshot().Mode; mode != "" {
		turnParams.ServiceTierForTurn = mode
	}
	// The session is durably active BEFORE the turn is submitted: the
	// harness may request input (or complete the turn) the instant it
	// accepts, and those handlers write their own logical state. Writing
	// "active" afterwards would overwrite a waiting_input that is already
	// durable.
	if err := a.setSessionState(m, session.StateActive); err != nil {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(err)
	}
	res, err := srv.TurnStart(callCtx, turnParams)
	if err != nil {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(fmt.Errorf("turn/start: %w%s", err, stderrSuffix(srv)))
	}
	if res.Turn.ID == "" {
		a.mu.Lock()
		st.current = nil
		a.mu.Unlock()
		return fail(errors.New("turn/start returned no turn id"))
	}
	a.mu.Lock()
	if st.current == nil {
		// The runtime died between acceptance and this write: the death
		// handler already failed the turn.
		a.mu.Unlock()
		return harness.PromptResult{}, fmt.Errorf("%w: runtime exited during turn submission",
			ErrRuntimeUnavailable)
	}
	st.current.turnID = res.Turn.ID
	st.current.agentText = ""
	st.current.agentItemID = ""
	turnID := st.current.turnID
	a.mu.Unlock()
	return harness.PromptResult{
		TurnID:          turnID,
		NativeSessionID: threadID,
		RuntimeID:       a.runtimeID(m.Session.ID),
	}, nil
}

// runtimeID returns the actual ephemeral identity of the runtime generation
// currently bound to the session — never the supervisor's stable key.
func (a *Adapter) runtimeID(sessionID string) string {
	v, ok := a.deps.Supervisor.View(sessionID)
	if !ok {
		return ""
	}
	return v.RuntimeID
}

// Cancel interrupts the in-flight turn of a session. It is serialized
// through the supervisor's mutation blocker so overlapping cancels fail
// with a stable error instead of racing.
func (a *Adapter) Cancel(ctx context.Context, m *session.Managed) error {
	st := a.state(m.Session.ID)
	if st == nil {
		return fmt.Errorf("session %s not tracked by the codex adapter", m.Session.ID)
	}
	a.mu.Lock()
	cur := st.current
	a.mu.Unlock()
	if cur == nil || cur.turnID == "" {
		return ErrNoActiveTurn
	}
	if err := a.deps.Supervisor.BeginMutation(m.Session.ID); err != nil {
		return err
	}
	defer a.deps.Supervisor.EndMutation(m.Session.ID)

	a.mu.Lock()
	srv := a.servers[RuntimeKey]
	a.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("%w: runtime gone", ErrNoActiveTurn)
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := srv.TurnInterrupt(callCtx, TurnInterruptParams{
		ThreadID: cur.nativeThreadID,
		TurnID:   cur.turnID,
	}); err != nil {
		return fmt.Errorf("turn/interrupt: %w", err)
	}
	// The terminal state arrives as turn/completed(status=interrupted) —
	// Relay does not synthesize it locally.
	return nil
}

// --- requested input ------------------------------------------------
