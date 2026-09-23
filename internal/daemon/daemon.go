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
	"github.com/rceman/reposuite-relay/internal/auth"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/events"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/presentation"
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

// Daemon owns the listener, the descriptor, the registry, the event
// broker, and all active generations.
type Daemon struct {
	paths paths.Paths
	opts  Options

	started      time.Time
	registry     *session.Registry
	store        *store.Sessions
	broker       *events.Broker
	supervisor   *runtime.Supervisor
	adapters     map[string]adapterEntry
	instanceID   string
	token        string
	machineToken string

	lockFile *os.File
	ln       net.Listener
	srv      *http.Server

	web    *webAuth              // browser-auth state; creds nil until admin setup
	webMu  sync.Mutex            // serializes the create-once admin credential commit
	static *webStatic            // embedded SvelteKit Web Admin build
	pres   *presentation.Manager // Relay-local project/grouping catalog

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
	// The durable config domain must be owner-private before any of it is
	// trusted. A pre-existing broader mode fails closed — Relay never
	// silently tightens a directory that may already have exposed a
	// credential.
	if err := config.RequirePrivateDir(p.ConfigDir()); err != nil {
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
	// Restart reconciliation: a durable input.requested with no terminal
	// record names a native request that died with the previous
	// generation. Abort it durably and normalize the session — or fail
	// closed before any listener or descriptor exists.
	if err := d.reconcileRestoredInputs(); err != nil {
		d.releaseLock()
		return nil, err
	}
	// Relay-local presentation authority: a missing catalog is an empty
	// one; a malformed or insecure presentation.json fails startup closed
	// — before any listener or descriptor names this generation.
	pres, err := presentation.NewManager(p.PresentationConfig(), d.opts.PresentationHooks)
	if err != nil {
		d.releaseLock()
		return nil, fmt.Errorf("presentation config: %w", err)
	}
	d.pres = pres
	// The stable control-plane endpoint is resolved under singleton
	// authority: first start binds a free loopback port and commits it to
	// config/relay.json atomically; later starts bind the configured port
	// exactly — never a fallback. The persistent machine API credential
	// is created or strictly loaded under the same authority.
	if err := d.resolveEndpoint(); err != nil {
		d.releaseLock()
		return nil, err
	}
	machineToken, err := auth.Ensure(d.paths.MachineToken())
	if err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
	d.machineToken = machineToken
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
	// Web Admin credential domain: admin.json absent means first-run setup
	// mode; malformed or insecure content fails startup closed. Browser
	// sessions live only in memory and die with this generation.
	web, err := newWebAuth(p, d.endpoint(), d.opts)
	if err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
	d.web = web
	// The embedded Web Admin build is part of the binary — its absence or
	// corruption is a build defect, so startup fails closed.
	d.static, err = newWebStatic()
	if err != nil {
		d.ln.Close()
		d.releaseLock()
		return nil, err
	}
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
	// Runtime watchers report unexpected exits asynchronously; their
	// OnRuntimeGone callbacks write durable state, so shutdown is not
	// quiescent until every watcher has returned. Adapter-owned goroutines
	// (transport readers, turn-completion workers) may likewise still be
	// finishing their final writes now that every process is reaped.
	d.supervisor.WaitWatchers()
	d.quiesceAdapters()
	return nil
}

// Shutdown asks a running daemon to stop gracefully (test seam; external
// users use POST /v1/daemon/shutdown or SIGTERM).
func (d *Daemon) Shutdown() { d.initiate() }

// InstanceID returns this daemon generation's identity (test seam).
func (d *Daemon) InstanceID() string { return d.instanceID }

// Token returns the local bearer token (test seam — never log or print).
func (d *Daemon) Token() string { return d.token }
