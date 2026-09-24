package daemon

import (
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- handlers ---------------------------------------------------------
func (d *Daemon) info() api.DaemonInfo {
	// Cheap in-memory projection only: a session counts as ACTIVE when a
	// live runtime generation is bound to it, otherwise COLD. Nothing is
	// woken and no transcript is opened to produce this.
	active := 0
	for _, m := range d.registry.List() {
		if _, ok := d.supervisor.View(m.Session.ID); ok {
			active++
		}
	}
	return api.DaemonInfo{
		InstanceID:     d.instanceID,
		PID:            os.Getpid(),
		APIVersion:     api.Version,
		UptimeSeconds:  time.Since(d.started).Seconds(),
		SessionCount:   d.registry.Len(),
		ActiveSessions: active,
		ColdSessions:   d.registry.Len() - active,
	}
}

func (d *Daemon) handleDaemonInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
	go d.initiate() // shutdown is deferred until the response is written
}

func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request) {
	resp := api.SessionList{Daemon: d.info()}
	for _, m := range d.registry.List() {
		resp.Sessions = append(resp.Sessions, d.sessionInfo(m))
	}
	writeJSON(w, http.StatusOK, resp)
}

// sessionInfo projects a managed session into the wire DTO. The domain
// package knows nothing about the API — projection lives here in the
// control layer. Runtime state comes from the RuntimeSupervisor; a
// session with no binding projects as COLD: runtimeId "", pid 0, no
// generation start time. Several sessions may legitimately share one
// runtime ID/PID/start time (a shared Codex app-server).
func (d *Daemon) sessionInfo(m *session.Managed) api.SessionInfo {
	s := m.Snapshot()
	info := api.SessionInfo{
		Key:             s.Key,
		SessionID:       s.ID,
		NativeSessionID: s.NativeSessionID,
		Harness:         s.Harness,
		Cwd:             s.Cwd,
		State:           s.State,
		Generation:      s.Generation,
		Model:           s.Model,
		Mode:            s.Mode,
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339),
		RuntimeState:    session.RuntimeCold,
		Activity:        runtime.ActivityIdle,
	}
	if v, ok := d.supervisor.View(s.ID); ok {
		info.RuntimeID = v.RuntimeID
		info.RuntimeState = v.State
		info.PID = v.PID
		info.GenerationStartedAt = v.StartedAt.UTC().Format(time.RFC3339)
		info.Activity = v.Activity
	}
	// Last-known metrics are in-memory only and must never wake a runtime.
	if a, ok := d.adapterFor(s.Harness); ok {
		info.Metrics = a.Metrics(s.ID)
	}
	// Derived presentation projection: longest matching project root over
	// the session cwd — read-time only, never persisted on the session.
	if p, ok := d.pres.Match(s.Cwd); ok {
		info.ProjectID = p.ID
		info.ProjectName = p.Name
	}
	return info
}

// handleServeFixture atomically reserves the key BEFORE spawning, so a
// duplicate or racing request can never create a transient extra process.
// Order: reserve → session ID → runtime ID → spawn → durable create →
// commit → event state. On any failure the reservation is cancelled, the
// child is reaped, and nothing durable remains.
func (d *Daemon) handleServeFixture(w http.ResponseWriter, r *http.Request, _ string) {
	var req api.FixtureRequest
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
	runtimeID, err := d.opts.RandRuntimeID()
	if err != nil {
		fail(http.StatusInternalServerError, api.ErrInternal, "runtime id: "+err.Error())
		return
	}
	rtKey := fixtureRuntimeKey(sessionID)
	rt, err := d.supervisor.Ensure(r.Context(), rtKey, session.HarnessFixture, runtimeID, false,
		func() (runtime.Harness, error) {
			h, err := d.opts.SpawnFixture(d.opts.SelfExe, req.Cwd)
			if err != nil {
				return nil, err
			}
			if d.opts.WrapStop != nil {
				return &wrappedHarness{
					Harness: h,
					stop:    d.opts.WrapStop(h.Stop),
				}, nil
			}
			return h, nil
		})
	if err != nil {
		fail(http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	// The fixture runtime is ready as soon as its process is spawned (no
	// initialization handshake) and it is dedicated: one per session.
	if err := d.supervisor.Bind(rt.Key, sessionID); err != nil {
		_ = d.supervisor.Stop(rt.Key)
		fail(http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	now := time.Now()
	rs := &session.RelaySession{
		ID:         sessionID,
		Key:        req.Key,
		Harness:    session.HarnessFixture,
		Cwd:        req.Cwd,
		State:      session.StateIdle,
		Generation: 1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := d.store.Create(rs); err != nil {
		// Persistence failed: no durable session — reap the runtime too.
		_ = d.supervisor.Stop(rt.Key)
		var ue *store.UncertainError
		if errors.As(err, &ue) {
			// The canonical create commit point may have passed — the
			// durable outcome is ambiguous. Fail closed: halt the daemon;
			// the next start reconciles against canonical disk state.
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
		// Reservation lost — should be unreachable; never leak the child
		// or a dangling durable session.
		_ = d.supervisor.Stop(rt.Key)
		_ = d.store.Delete(rs.ID)
		d.registry.Cancel(req.Key)
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "lost key reservation")
		return
	}
	if _, err := d.broker.Ensure(m); err != nil {
		// Event state must exist for every managed session; a failure here
		// means the durable transcript is unusable — fail closed.
		fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
		go d.initiate()
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "event state: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}

// materialize applies a durable session metadata mutation requested by a
// harness adapter. It is the ONLY path by which native identity,
// generation, model/mode, and logical state become durable — always under
// the session's MetaMu, so a concurrent seq-block reservation (which uses
// the same mutex) can never interleave a partial write.
func (d *Daemon) materialize(m *session.Managed, upd harness.SessionUpdate) error {
	m.MetaMu.Lock()
	defer m.MetaMu.Unlock()
	rs := m.Session
	changed := false
	if upd.NativeSessionID != nil && rs.NativeSessionID != *upd.NativeSessionID {
		rs.NativeSessionID = *upd.NativeSessionID
		changed = true
	}
	if upd.Model != nil && rs.Model != *upd.Model {
		rs.Model = *upd.Model
		changed = true
	}
	if upd.Mode != nil && rs.Mode != *upd.Mode {
		rs.Mode = *upd.Mode
		changed = true
	}
	if upd.State != nil && rs.State != *upd.State {
		rs.State = *upd.State
		changed = true
	}
	if upd.BumpGeneration {
		rs.Generation++
		changed = true
	}
	// Telemetry sequence reservation is monotone-only: a lower watermark
	// is never written backward.
	if upd.TelemetrySeqWatermark != nil &&
		*upd.TelemetrySeqWatermark > rs.TelemetrySeqWatermark {
		rs.TelemetrySeqWatermark = *upd.TelemetrySeqWatermark
		changed = true
	}
	if !changed {
		return nil // no durable write for a no-op projection
	}
	rs.UpdatedAt = time.Now().UTC()
	return d.store.Save(rs)
}

// fixtureRuntimeKey is the supervisor key of a fixture session's dedicated
// runtime: one process per session, never shared.
func fixtureRuntimeKey(sessionID string) string { return "fixture/" + sessionID }

// wrappedHarness decorates a harness's stop path — the seam tests use to
// pause an in-flight stop under shutdown.
type wrappedHarness struct {
	runtime.Harness
	stop func() error
}

func (w *wrappedHarness) Stop() error { return w.stop() }

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionResponse{Daemon: d.info(), Session: d.sessionInfo(m)})
}
