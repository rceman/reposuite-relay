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
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
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
	// CodexCommand resolves the app-server command. Production resolves
	// the installed Codex CLI; tests point at the deterministic fake
	// app-server (the `__fake-codex` mode).
	CodexCommand func() (codex.Command, error)
	// OpenCodeCommand resolves the `opencode acp` command. Production
	// resolves the installed CLI; tests point at the deterministic fake ACP
	// agent (the `__fake-acp` mode).
	OpenCodeCommand func() (acp.Command, error)
	// DevinCommand resolves the `devin acp` command for a process-level
	// model. Production resolves the installed CLI; tests point at the
	// deterministic fake ACP agent.
	DevinCommand func(model string) (acp.Command, error)
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
	return o.withAdapterDefaults()
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
	adapters   map[string]adapterEntry
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
		adapters: map[string]adapterEntry{},
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
	d.registerAdapters()
	// Runtime removal is authoritative: the owning adapter fails in-flight
	// work durably on an unexpected exit, and every bound session becomes
	// COLD in either case (deliberate stop or death).
	d.supervisor.OnGone = d.onRuntimeGone
	for _, m := range reg.List() {
		if a, ok := d.adapterFor(m.Snapshot().Harness); ok {
			a.Track(m)
		}
	}
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
