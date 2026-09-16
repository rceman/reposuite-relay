// Package daemon implements the single persistent RepoSuite Relay daemon:
// an advisory-locked singleton per RepoSuite state root serving the
// ADR-006 local control plane — loopback HTTP/JSON commands plus an NDJSON
// canonical event stream — with runtime discovery through a user-private
// run/daemon.json descriptor.
//
// Singleton authority: an exclusive flock(2) on <relay>/run/relayd.lock.
// Concurrent `__daemon` contenders race for it; exactly one wins and the
// rest exit. The listener is bound and the descriptor published only
// after the lock is held; stale-descriptor cleanup is performed only by
// the lock owner, and shutdown removes the descriptor only when it still
// belongs to this daemon instance.
package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// Bounds for the local control plane — phase-specific, never a global
// write deadline (NDJSON event streams are intentionally long-lived).
const (
	ReadHeaderTimeout = 5 * time.Second  // request header deadline
	IdleTimeout       = 60 * time.Second // keep-alive idle deadline
	WriteTimeout      = 10 * time.Second // per-response deadline (streams clear it)
	MaxRequestSize    = 64 << 10         // bounded JSON request bodies

	// QuiescenceTimeout bounds how long shutdown waits for already-running
	// lifecycle handlers (create/stop ops) to finish before the final
	// registry drain. Never unbounded, and never skipped — the drain must
	// not race lifecycle ownership.
	QuiescenceTimeout = 10 * time.Second
)

// Options configures a Daemon. Zero fields take production defaults; the
// seams exist so tests can count spawns, inject failures, and shorten
// deadlines deterministically without changing production behavior.
type Options struct {
	SelfExe      string // path used to spawn __fixture children
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	SpawnFixture func(selfExe, cwd string) (runtime.Harness, error)
	// Separate identity seams: the durable RelaySession ID and the
	// ephemeral HarnessRuntime ID are different semantics and never share
	// one value.
	RandSessionID func() (string, error)
	RandRuntimeID func() (string, error)
	// WrapStop decorates each session's runtime-stop function — the seam
	// tests use to pause an in-flight stop under shutdown.
	WrapStop func(stop func() error) func() error
	// StoreHooks overrides store filesystem primitives — test seam only
	// for deterministic post-commit failure injection.
	StoreHooks *store.Hooks
}

func (o Options) withDefaults() Options {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = ReadHeaderTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = WriteTimeout
	}
	if o.SpawnFixture == nil {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
			return fixture.Spawn(selfExe, cwd)
		}
	}
	if o.RandSessionID == nil {
		o.RandSessionID = session.NewSessionID
	}
	if o.RandRuntimeID == nil {
		o.RandRuntimeID = session.NewRuntimeID
	}
	return o
}

// Daemon owns the listener, the descriptor, the registry, the event
// broker, and all active generations.
type Daemon struct {
	paths paths.Paths
	opts  Options

	started    time.Time
	registry   *session.Registry
	store      *store.Sessions
	broker     *events.Broker
	supervisor *runtime.Supervisor
	instanceID string
	token      string

	lockFile *os.File
	ln       net.Listener
	srv      *http.Server

	mu       sync.Mutex
	shutting bool
	shutdown chan struct{}
	once     sync.Once
	wg       sync.WaitGroup // in-flight lifecycle handlers (create/stop)
	conns    map[net.Conn]http.ConnState
}

// ErrAlreadyRunning is returned when another daemon owns this state root.
var ErrAlreadyRunning = errors.New("another daemon owns this state root")

// Run is the __daemon entry point: acquire singleton lock, recover the
// durable store, bind loopback, publish the descriptor, serve until
// shutdown/signal, then clean up.
func Run(p paths.Paths, selfExe string) error {
	d, err := Start(p, Options{SelfExe: selfExe})
	if err != nil {
		return err
	}
	return d.Serve()
}

// Start performs singleton lock acquisition, durable-store load/recovery,
// registry restore, event-state initialization, loopback bind, and
// descriptor publication, then returns a Daemon ready to Serve. Order
// preserves ownership safety: only the singleton owner mutates or
// recovers the store, and any failure releases the lock without leaving a
// listener or descriptor behind. Splitting Start/Serve lets tests inject
// seams via Options before serving begins.
func Start(p paths.Paths, opts Options) (*Daemon, error) {
	if err := p.Ensure(); err != nil {
		return nil, err
	}
	d := &Daemon{
		paths:    p,
		opts:     opts.withDefaults(),
		started:  time.Now(),
		shutdown: make(chan struct{}),
		conns:    map[net.Conn]http.ConnState{},
	}
	if err := d.acquireLock(); err != nil {
		return nil, err
	}
	ss, err := store.OpenSessions(p.SessionsDir(), d.opts.RandSessionID)
	if err != nil {
		d.releaseLock()
		return nil, err
	}
	if d.opts.StoreHooks != nil {
		ss.SetHooks(d.opts.StoreHooks)
	}
	loaded, err := ss.LoadAll()
	if err != nil {
		d.releaseLock()
		return nil, fmt.Errorf("session store: %w", err)
	}
	reg, err := session.NewRestored(loaded)
	if err != nil {
		d.releaseLock()
		return nil, fmt.Errorf("session store: %w", err)
	}
	d.store = ss
	d.registry = reg
	d.broker = events.NewBroker(ss)
	d.supervisor = runtime.New()
	// Event state for every restored session: open transcripts via the
	// healthy O(1) path and validate the durable seq watermark.
	for _, m := range reg.List() {
		if _, err := d.broker.Ensure(m); err != nil {
			d.releaseLock()
			return nil, fmt.Errorf("session store: %w", err)
		}
	}
	if err := d.bindListener(); err != nil {
		d.releaseLock()
		return nil, err
	}
	instanceID, err := newInstanceID()
	if err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
	d.instanceID = instanceID
	d.token = token
	d.srv = &http.Server{
		Handler:           http.HandlerFunc(d.route),
		ReadHeaderTimeout: d.opts.ReadTimeout,
		IdleTimeout:       IdleTimeout,
		ConnState:         d.connState,
	}
	// Descriptor publication IS daemon readiness: lock held, store
	// recovered, listener bound, token generated, handler constructed.
	if err := writeDescriptor(p, api.Descriptor{
		Version:     api.DescriptorVersion,
		InstanceID:  d.instanceID,
		PID:         os.Getpid(),
		Endpoint:    d.endpoint(),
		BearerToken: d.token,
	}); err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
	return d, nil
}

func (d *Daemon) endpoint() string { return "http://" + d.ln.Addr().String() }

// Serve runs the HTTP server until shutdown or SIGTERM/SIGINT, then
// disconnects event subscribers, drains lifecycle handlers, and stops
// every live runtime. Shutdown stops runtimes only — durable RelaySessions
// are retained and reload as COLD on the next start. A session belongs to
// Relay until explicit deletion.
//
// Shutdown is a quiescence barrier, in order:
//  1. mark shutting down (mutating handlers are rejected, in-flight ones
//     are already counted in wg)
//  2. disconnect all event subscribers — long-lived streams end, so the
//     HTTP server can actually finish draining
//  3. http.Server.Shutdown: stop accepting, close the listener, wait for
//     in-flight handlers (bounded by QuiescenceTimeout)
//  4. bounded belt-and-suspenders wait for lifecycle handlers
//  5. only then drain the registry and stop remaining generations —
//     never concurrently with a create/stop lifecycle operation
func (d *Daemon) Serve() error {
	defer removeOwnDescriptor(d.paths, d.instanceID)
	defer d.releaseLock()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := d.srv.Serve(d.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "relayd: serve: %v\n", err)
		}
	}()

	select {
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "relayd: signal %s, shutting down\n", sig)
	case <-d.shutdown:
	}

	d.initiate()
	d.broker.CloseAll()
	// Close connections that never started (or finished) a request so they
	// cannot delay shutdown until the header deadline; connections with a
	// request in flight are left to the graceful Shutdown below.
	d.closeIdleConns()

	ctx, cancel := context.WithTimeout(context.Background(), QuiescenceTimeout)
	err := d.srv.Shutdown(ctx)
	cancel()
	if err != nil {
		// Forced close: remaining connections are dropped.
		_ = d.srv.Close()
	}
	<-serveDone

	drained := make(chan struct{})
	go func() { d.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(QuiescenceTimeout):
		// Do not drain: a live handler may still own a generation's Stop.
		// Process exit is the final safety net — fixture children die via
		// stdin-pipe EOF when this process exits.
		qerr := fmt.Errorf("relayd: handler quiescence exceeded %s", QuiescenceTimeout)
		fmt.Fprintln(os.Stderr, qerr)
		return qerr
	}

	d.registry.Drain()
	// Runtime authority: stop every harness process tree (dedicated and
	// shared) with bounded, confirmed reaping. Durable RelaySessions stay.
	if err := d.supervisor.StopAll(); err != nil {
		fmt.Fprintf(os.Stderr, "relayd: stop runtimes: %v\n", err)
	}
	return nil
}

// Shutdown asks a running daemon to stop gracefully (test seam; external
// users use POST /v1/daemon/shutdown or SIGTERM).
func (d *Daemon) Shutdown() { d.initiate() }

// InstanceID returns this daemon generation's identity (test seam).
func (d *Daemon) InstanceID() string { return d.instanceID }

// Token returns the local bearer token (test seam — never log or print).
func (d *Daemon) Token() string { return d.token }

// acquireLock takes the exclusive non-blocking advisory lock that makes the
// daemon a singleton. Stale lock FILES are harmless: the kernel lock, not
// file existence, is the authority.
func (d *Daemon) acquireLock() error {
	lockPath := d.paths.DaemonLock()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("daemon lock: %w", err)
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		f.Close()
		return fmt.Errorf("daemon lock chmod: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return ErrAlreadyRunning
	}
	d.lockFile = f
	return nil
}

func (d *Daemon) releaseLock() {
	_ = syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN)
	d.lockFile.Close()
}

// bindListener binds the loopback control endpoint. The OS chooses the
// ephemeral port; anything that is not loopback fails startup closed.
func (d *Daemon) bindListener() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback control endpoint: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
		ln.Close()
		return fmt.Errorf("listener %s is not loopback; refusing startup", ln.Addr())
	}
	d.ln = ln
	return nil
}

// connState tracks connection phases so shutdown can close idle
// connections promptly without disturbing in-flight requests.
func (d *Daemon) connState(c net.Conn, st http.ConnState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch st {
	case http.StateClosed, http.StateHijacked:
		delete(d.conns, c)
	default:
		d.conns[c] = st
	}
}

// closeIdleConns closes connections with no request in flight.
func (d *Daemon) closeIdleConns() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for c, st := range d.conns {
		if st == http.StateNew || st == http.StateIdle {
			_ = c.Close()
		}
	}
}

// initiate begins shutdown exactly once.
func (d *Daemon) initiate() {
	d.once.Do(func() {
		d.mu.Lock()
		d.shutting = true
		d.mu.Unlock()
		close(d.shutdown)
	})
}

func (d *Daemon) isShutting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shutting
}

// --- routing / auth ---------------------------------------------------

// route dispatches one request. Every route requires the bearer token,
// including daemon status, transcript, the event stream, and shutdown.
func (d *Daemon) route(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		writeErr(w, http.StatusUnauthorized, api.ErrUnauthorized, "missing or invalid bearer token")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/v1/daemon" && r.Method == http.MethodGet:
		d.handleDaemonInfo(w, r)
	case p == "/v1/daemon/shutdown" && r.Method == http.MethodPost:
		d.handleShutdown(w, r)
	case p == "/v1/sessions" && r.Method == http.MethodGet:
		d.handleList(w, r)
	case p == "/v1/sessions/fixture" && r.Method == http.MethodPost:
		d.lifecycle(d.handleServeFixture)(w, r, "")
	case strings.HasPrefix(p, "/v1/sessions/"):
		d.routeSession(w, r, strings.TrimPrefix(p, "/v1/sessions/"))
	default:
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
	}
}

// routeSession handles /v1/sessions/{key}[/transcript|/events].
func (d *Daemon) routeSession(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "":
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
	case strings.HasSuffix(rest, "/transcript"):
		key := strings.TrimSuffix(rest, "/transcript")
		if strings.Contains(key, "/") || r.Method != http.MethodGet {
			methodOrNotFound(w, r, http.MethodGet)
			return
		}
		d.handleTranscript(w, r, key)
	case strings.HasSuffix(rest, "/events"):
		key := strings.TrimSuffix(rest, "/events")
		if strings.Contains(key, "/") || r.Method != http.MethodGet {
			methodOrNotFound(w, r, http.MethodGet)
			return
		}
		d.handleEvents(w, r, key)
	case !strings.Contains(rest, "/"):
		switch r.Method {
		case http.MethodGet:
			d.handleStatus(w, r, rest)
		case http.MethodDelete:
			d.lifecycle(d.handleStop)(w, r, rest)
		default:
			methodOrNotFound(w, r, http.MethodGet, http.MethodDelete)
		}
	default:
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
	}
}

func methodOrNotFound(w http.ResponseWriter, r *http.Request, allowed ...string) {
	for _, m := range allowed {
		if r.Method == m {
			writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
			return
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeErr(w, http.StatusMethodNotAllowed, api.ErrInvalidRequest, "method not allowed")
}

// authorized performs constant-time bearer token comparison.
func (d *Daemon) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := h[len(prefix):]
	if len(got) != len(d.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.token)) == 1
}

// lifecycle admits a mutating handler through the shutdown barrier: once
// shutting down no new lifecycle work starts, and every admitted handler
// is counted for the quiescence wait.
func (d *Daemon) lifecycle(h func(http.ResponseWriter, *http.Request, string)) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, key string) {
		d.mu.Lock()
		if d.shutting {
			d.mu.Unlock()
			writeErr(w, http.StatusServiceUnavailable, api.ErrShuttingDown, "daemon is shutting down")
			return
		}
		d.wg.Add(1)
		d.mu.Unlock()
		defer d.wg.Done()
		h(w, r, key)
	}
}

// --- response helpers -------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.ErrorBody{Error: api.ErrorDetail{Code: code, Message: msg}})
}

// decodeBody strictly decodes a bounded JSON object body: 64 KiB cap,
// unknown fields rejected, exactly one JSON value.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestSize)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

// --- handlers ---------------------------------------------------------

func (d *Daemon) info() api.DaemonInfo {
	return api.DaemonInfo{
		InstanceID:    d.instanceID,
		PID:           os.Getpid(),
		APIVersion:    api.Version,
		UptimeSeconds: time.Since(d.started).Seconds(),
		SessionCount:  d.registry.Len(),
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
	s := m.Session
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
	rt, err := d.supervisor.Ensure(rtKey, session.HarnessFixture, runtimeID, false,
		func() (runtime.Harness, error) {
			h, err := d.opts.SpawnFixture(d.opts.SelfExe, req.Cwd)
			if err != nil {
				return nil, err
			}
			if d.opts.WrapStop != nil {
				return &wrappedHarness{Harness: h, stop: d.opts.WrapStop(h.Stop)}, nil
			}
			return h, nil
		})
	if err != nil {
		fail(http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	// A fixture runtime is ready as soon as the process is spawned (no
	// initialization handshake), and it is dedicated: one per session.
	if err := d.supervisor.MarkReady(rt.Key); err != nil {
		_ = d.supervisor.Stop(rt.Key)
		fail(http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
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

// handleStop takes exclusive stop ownership (BeginStop), stops and
// detaches the runtime if one exists (ClearRuntime → COLD), durably
// deletes the session, then removes it (CommitStop) and closes its event
// subscribers. A failed runtime stop keeps the session as-is
// (AbortStop); a failed durable delete after a confirmed runtime stop
// keeps the session managed but COLD — the daemon never loses authority
// and never retains a dead PID.
func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.BeginStop(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	// Runtime ownership: a dedicated runtime (fixture) is stopped and
	// reaped; a shared runtime (Codex) is only detached — deleting one
	// session never kills a runtime other sessions still use.
	if err := d.supervisor.StopSession(m.Session.ID); err != nil {
		d.registry.AbortStop(key)
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "stop runtime: "+err.Error())
		return
	}
	if err := d.store.Delete(m.Session.ID); err != nil {
		var ce *store.CleanupError
		var ue *store.UncertainError
		switch {
		case errors.As(err, &ce):
			// Deletion committed — apply it. The leftover tombstone is
			// recovered on next daemon start.
			fmt.Fprintf(os.Stderr, "relayd: %v\n", err)
			d.registry.CommitStop(key)
			d.broker.Remove(m.Session.ID)
			writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
			return
		case errors.As(err, &ue):
			// Namespace outcome committed but durability is ambiguous —
			// apply the outcome, then fail closed and halt.
			d.registry.CommitStop(key)
			d.broker.Remove(m.Session.ID)
			go d.initiate()
			writeErr(w, http.StatusInternalServerError, api.ErrInternal,
				"store commit uncertain; daemon halting: "+err.Error())
			return
		default:
			d.registry.AbortStop(key)
			writeErr(w, http.StatusInternalServerError, api.ErrInternal, "delete session: "+err.Error())
			return
		}
	}
	d.registry.CommitStop(key)
	d.broker.Remove(m.Session.ID)
	writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
}

// handleTranscript serves bounded durable transcript tail records through
// the derived index — durable records, not rendered rows.
func (d *Daemon) handleTranscript(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	limit := api.TranscriptDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > api.TranscriptMaxLimit {
		limit = api.TranscriptMaxLimit
	}
	evs, err := d.broker.Ensure(m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	page, err := evs.HistorySnapshot(limit, api.HistoryPageMaxBytes)
	if err != nil {
		if errors.Is(err, store.ErrRecordTooLarge) {
			writeErr(w, http.StatusInternalServerError, api.ErrHistoryRecordTooLarge,
				"canonical record exceeds the bounded history page")
			return
		}
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "read transcript: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.TranscriptPage{
		ThroughSeq:    page.ThroughSeq,
		Records:       page.Records,
		HasMoreBefore: page.HasMoreBefore,
	})
}

// handleEvents streams canonical events as NDJSON: the atomic replay
// prefix (events after the requested cursor) followed by live events. The
// cursor-too-old case is a normal JSON error before any streaming begins.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request, key string) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return
	}
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid after cursor")
			return
		}
		after = n
	}
	evs, err := d.broker.Ensure(m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	sub, err := evs.Subscribe(after)
	if err != nil {
		switch {
		case errors.Is(err, events.ErrCursorTooOld):
			writeErr(w, http.StatusConflict, api.ErrCursorTooOld,
				"requested cursor is older than the replay window; rehydrate durable history")
			return
		case errors.Is(err, events.ErrCursorAhead):
			writeErr(w, http.StatusConflict, api.ErrCursorAhead,
				"requested cursor is ahead of the current event cursor")
			return
		}
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
		return
	}
	defer evs.Unsubscribe(sub)

	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // streams are intentionally long-lived
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() { _ = rc.Flush() }
	for _, ev := range sub.Replay {
		_ = enc.Encode(ev)
	}
	flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				if reason := sub.Err(); reason != nil {
					code := "STREAM_CLOSED"
					if errors.Is(reason, events.ErrSubscriberEvicted) {
						code = "SUBSCRIBER_EVICTED"
					}
					_ = enc.Encode(api.ErrorBody{Error: api.ErrorDetail{
						Code: code, Message: reason.Error(),
					}})
					flush()
				}
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			flush()
		}
	}
}
