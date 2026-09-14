// Package daemon implements the single persistent RepoSuite Relay daemon:
// an advisory-locked singleton per RepoSuite state root serving a bounded
// versioned JSON protocol on a private Unix socket.
//
// Singleton authority: an exclusive flock(2) on <relay>/run/relayd.lock.
// Concurrent `__daemon` contenders race for it; exactly one wins and the
// rest exit. The socket is bound only after the lock is held, and stale
// socket cleanup is performed only by the lock owner.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/protocol"
	"github.com/rceman/reposuite-relay/internal/session"
)

// LockName is the daemon singleton lock file inside the run directory.
const LockName = "relayd.lock"

// Bounds for the local control plane — phase-specific deadlines.
const (
	ReadTimeout  = 5 * time.Second // per-connection read deadline
	WriteTimeout = 5 * time.Second // per-connection write deadline

	// QuiescenceTimeout bounds how long shutdown waits for already-running
	// request handlers (create/stop lifecycle ops) to finish before the
	// final registry drain. Never unbounded, and never skipped — the drain
	// must not race lifecycle ownership.
	QuiescenceTimeout = 10 * time.Second
)

// Options configures a Daemon. Zero fields take production defaults; the
// seams exist so tests can count spawns, inject failures, and shorten
// deadlines deterministically without changing production behavior.
type Options struct {
	SelfExe      string // path used to spawn __fixture children
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	SpawnFixture func(selfExe, cwd string) (*fixture.Child, error)
	RandID       func() (string, error)
	// WrapStop decorates each session's generation-stop function — the seam
	// tests use to pause an in-flight stop under shutdown.
	WrapStop func(stop func() error) func() error
}

func (o Options) withDefaults() Options {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = ReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = WriteTimeout
	}
	if o.SpawnFixture == nil {
		o.SpawnFixture = fixture.Spawn
	}
	if o.RandID == nil {
		o.RandID = session.NewRuntimeID
	}
	return o
}

// Daemon owns the listener, the registry, and all active generations.
type Daemon struct {
	paths paths.Paths
	opts  Options

	started  time.Time
	registry *session.Registry

	lockFile *os.File
	ln       net.Listener

	mu       sync.Mutex
	shutting bool
	conns    map[net.Conn]struct{}
	shutdown chan struct{}
	once     sync.Once
	wg       sync.WaitGroup // in-flight request handlers (lifecycle ops)
}

// ErrAlreadyRunning is returned when another daemon owns this state root.
var ErrAlreadyRunning = errors.New("another daemon owns this state root")

// Run is the __daemon entry point: acquire singleton lock, recover stale
// socket, serve until shutdown/signal, then clean up.
func Run(p paths.Paths, selfExe string) error {
	d, err := Start(p, Options{SelfExe: selfExe})
	if err != nil {
		return err
	}
	return d.Serve()
}

// Start performs singleton lock acquisition, stale-socket recovery, and the
// socket bind, then returns a Daemon ready to Serve. Splitting Start/Serve
// lets tests inject seams via Options before the accept loop begins.
func Start(p paths.Paths, opts Options) (*Daemon, error) {
	if err := p.Ensure(); err != nil {
		return nil, err
	}
	d := &Daemon{
		paths:    p,
		opts:     opts.withDefaults(),
		started:  time.Now(),
		registry: session.NewRegistry(),
		conns:    map[net.Conn]struct{}{},
		shutdown: make(chan struct{}),
	}
	if err := d.acquireLock(); err != nil {
		return nil, err
	}
	if err := d.bindSocket(); err != nil {
		d.releaseLock()
		return nil, err
	}
	return d, nil
}

// Serve runs the accept loop until shutdown or SIGTERM/SIGINT, then stops
// every fixture generation, closes the listener, and removes the socket.
//
// Shutdown is a quiescence barrier, in order:
//  1. mark shutting down (no new dispatch work)
//  2. close the listener — stops new connections
//  3. wait for the accept loop to die — no more handler Adds are possible
//  4. close tracked connections — unblocks idle readers (a handler blocked
//     in a lifecycle op is unaffected; it is not conn-bound)
//  5. wait for in-flight handlers, bounded by QuiescenceTimeout
//  6. only then drain the registry and stop remaining generations —
//     never concurrently with a create/stop lifecycle operation
func (d *Daemon) Serve() error {
	defer os.Remove(d.paths.DaemonSocket())
	defer d.releaseLock()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	acceptDone := make(chan struct{})
	go func() { d.acceptLoop(); close(acceptDone) }()

	select {
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "relayd: signal %s, shutting down\n", sig)
	case <-d.shutdown:
	}

	d.initiate()
	_ = d.ln.Close()
	<-acceptDone

	d.mu.Lock()
	for c := range d.conns {
		_ = c.Close()
	}
	d.mu.Unlock()

	drained := make(chan struct{})
	go func() { d.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(QuiescenceTimeout):
		// Do not drain: a live handler may still own a generation's Stop.
		// Process exit is the final safety net — fixture children die via
		// stdin-pipe EOF when this process exits.
		err := fmt.Errorf("relayd: handler quiescence exceeded %s", QuiescenceTimeout)
		fmt.Fprintln(os.Stderr, err)
		return err
	}

	for _, m := range d.registry.Drain() {
		if err := m.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "relayd: stop %s: %v\n", m.Session.Key, err)
		}
	}
	return nil
}

// Shutdown asks a running daemon to stop gracefully (test seam; external
// users normally use the `shutdown` protocol op or SIGTERM).
func (d *Daemon) Shutdown() { d.initiate() }

// acquireLock takes the exclusive non-blocking advisory lock that makes the
// daemon a singleton. Stale lock FILES are harmless: the kernel lock, not
// file existence, is the authority.
func (d *Daemon) acquireLock() error {
	lockPath := filepath.Join(d.paths.RunDir(), LockName)
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

// bindSocket binds the daemon socket. Only called while holding the lock:
// the lock owner may remove a stale Unix socket inode, but never deletes a
// non-socket object at that path — that fails closed instead.
func (d *Daemon) bindSocket() error {
	sock := d.paths.DaemonSocket()
	fi, err := os.Lstat(sock)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket (mode %s); refusing to remove it", sock, fi.Mode())
		}
		if err := os.Remove(sock); err != nil { // stale socket inode
			return fmt.Errorf("remove stale socket: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("stat socket: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("bind %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("socket chmod: %w", err)
	}
	d.ln = ln
	return nil
}

func (d *Daemon) acceptLoop() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return // listener closed on shutdown
		}
		d.mu.Lock()
		if d.shutting {
			d.mu.Unlock()
			_ = c.Close()
			continue
		}
		d.conns[c] = struct{}{}
		d.wg.Add(1) // before the goroutine; no Adds survive acceptDone
		d.mu.Unlock()
		go d.serveConn(c)
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

// serveConn handles one request/response with bounded phase-specific
// deadlines, then closes the connection: ReadTimeout applies while reading
// the request, WriteTimeout while writing the response.
func (d *Daemon) serveConn(c net.Conn) {
	defer func() {
		d.wg.Done()
		d.mu.Lock()
		delete(d.conns, c)
		d.mu.Unlock()
		_ = c.Close()
	}()

	_ = c.SetReadDeadline(time.Now().Add(d.opts.ReadTimeout))
	limited := io.LimitReader(c, protocol.MaxRequest+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return // idle/short-lived client: read deadline or close
	}
	var resp protocol.Response
	switch {
	case len(raw) > protocol.MaxRequest:
		resp = protocol.Fail(protocol.ErrInvalidRequest, "request exceeds 64KiB")
	case len(raw) == 0:
		return // client sent nothing
	default:
		resp = d.dispatch(raw)
	}
	_ = c.SetWriteDeadline(time.Now().Add(d.opts.WriteTimeout))
	_ = json.NewEncoder(c).Encode(resp)

	// shutdown is deferred until the response has been written.
	if resp.OK && isShutdown(raw) {
		go d.initiate()
	}
}

func isShutdown(raw []byte) bool {
	var probe struct {
		Op string `json:"op"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.Op == protocol.OpShutdown
}

// dispatch validates and executes one request.
func (d *Daemon) dispatch(raw []byte) protocol.Response {
	var req protocol.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return protocol.Fail(protocol.ErrInvalidRequest, "malformed JSON: "+err.Error())
	}
	if req.Version != protocol.Version {
		return protocol.Fail(protocol.ErrProtocolMismatch,
			fmt.Sprintf("protocol version %d, daemon speaks %d", req.Version, protocol.Version))
	}
	if d.isShutting() && req.Op != protocol.OpPing && req.Op != protocol.OpDaemonStatus {
		return protocol.Fail(protocol.ErrShuttingDown, "daemon is shutting down")
	}
	switch req.Op {
	case protocol.OpPing, protocol.OpDaemonStatus:
		return protocol.Ok(d.info())
	case protocol.OpServeFixture:
		return d.serveFixture(req)
	case protocol.OpListSessions:
		r := protocol.Ok(d.info())
		for _, m := range d.registry.List() {
			r.Sessions = append(r.Sessions, m.Info())
		}
		return r
	case protocol.OpSessionStatus:
		return d.sessionStatus(req)
	case protocol.OpStopSession:
		return d.stopSession(req)
	case protocol.OpShutdown:
		return protocol.Ok(d.info())
	default:
		return protocol.Fail(protocol.ErrInvalidRequest, "unknown op "+req.Op)
	}
}

func (d *Daemon) isShutting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shutting
}

func (d *Daemon) info() *protocol.DaemonInfo {
	return &protocol.DaemonInfo{
		PID:             os.Getpid(),
		ProtocolVersion: protocol.Version,
		UptimeSeconds:   time.Since(d.started).Seconds(),
		SessionCount:    d.registry.Len(),
	}
}

// serveFixture atomically reserves the key BEFORE spawning, so a duplicate
// or racing request can never create a transient extra process. On any
// failure the reservation is cancelled and nothing remains.
func (d *Daemon) serveFixture(req protocol.Request) protocol.Response {
	if !session.ValidKey(req.Key) {
		return protocol.Fail(protocol.ErrInvalidSessionKey, "invalid session key "+req.Key)
	}
	cwd := req.Cwd
	if cwd == "" || !filepath.IsAbs(cwd) {
		return protocol.Fail(protocol.ErrInvalidRequest, "serve requires an absolute client cwd")
	}
	if !d.registry.Reserve(req.Key) {
		return protocol.Fail(protocol.ErrSessionExists, "session "+req.Key+" already exists")
	}
	fail := func(r protocol.Response) protocol.Response {
		d.registry.Cancel(req.Key)
		return r
	}
	runtimeID, err := d.opts.RandID()
	if err != nil {
		return fail(protocol.Fail(protocol.ErrInternal, "runtime id: "+err.Error()))
	}
	child, err := d.opts.SpawnFixture(d.opts.SelfExe, cwd)
	if err != nil {
		return fail(protocol.Fail(protocol.ErrFixtureStart, err.Error()))
	}
	stop := child.Stop
	if d.opts.WrapStop != nil {
		stop = d.opts.WrapStop(stop)
	}
	now := time.Now()
	m := &session.Managed{
		Session: &session.Session{
			Key:        req.Key,
			RuntimeID:  runtimeID,
			Harness:    session.HarnessFixture,
			Cwd:        cwd,
			State:      session.StateRunning,
			CreatedAt:  now,
			Generation: 1,
		},
		Generation: &session.ActiveGeneration{PID: child.PID(), StartedAt: now},
		Stop:       stop,
	}
	if !d.registry.Commit(req.Key, m) {
		// Reservation lost — should be unreachable; never leak the child.
		_ = child.Stop()
		d.registry.Cancel(req.Key)
		return protocol.Fail(protocol.ErrInternal, "lost key reservation")
	}
	r := protocol.Ok(d.info())
	r.Session = ptr(m.Info())
	return r
}

func (d *Daemon) sessionStatus(req protocol.Request) protocol.Response {
	if !session.ValidKey(req.Key) {
		return protocol.Fail(protocol.ErrInvalidSessionKey, "invalid session key "+req.Key)
	}
	m, ok := d.registry.Get(req.Key)
	if !ok {
		return protocol.Fail(protocol.ErrSessionNotFound, "no session "+req.Key)
	}
	r := protocol.Ok(d.info())
	r.Session = ptr(m.Info())
	return r
}

// stopSession takes exclusive stop ownership (BeginStop), stops the
// generation, and only then removes the session (CommitStop). A failed Stop
// keeps the session in the registry (AbortStop) — the daemon never loses
// authority over a still-running generation.
func (d *Daemon) stopSession(req protocol.Request) protocol.Response {
	if !session.ValidKey(req.Key) {
		return protocol.Fail(protocol.ErrInvalidSessionKey, "invalid session key "+req.Key)
	}
	m, ok := d.registry.BeginStop(req.Key)
	if !ok {
		return protocol.Fail(protocol.ErrSessionNotFound, "no session "+req.Key)
	}
	if err := m.Stop(); err != nil {
		d.registry.AbortStop(req.Key)
		return protocol.Fail(protocol.ErrInternal, "stop fixture: "+err.Error())
	}
	d.registry.CommitStop(req.Key)
	return protocol.Ok(d.info())
}

func ptr[T any](v T) *T { return &v }
