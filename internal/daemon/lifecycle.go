package daemon

// Daemon lifecycle: endpoint accessor, Serve quiescence barrier,
// Shutdown initiation, and identity getters.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

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
	// Telemetry drain is bounded: pending events get a final delivery
	// attempt, then move to the outage spool — never a shutdown hang.
	d.telemetry.Shutdown()
	return nil
}

// Shutdown asks a running daemon to stop gracefully (test seam; external
// users use POST /v1/daemon/shutdown or SIGTERM).
func (d *Daemon) Shutdown() { d.initiate() }

// InstanceID returns this daemon generation's identity (test seam).
func (d *Daemon) InstanceID() string { return d.instanceID }

// Token returns the local bearer token (test seam — never log or print).
func (d *Daemon) Token() string { return d.token }
