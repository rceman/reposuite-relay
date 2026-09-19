package daemon

import (
	"context"
	"errors"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"net/http"
)

// adapterEntry is one registered native harness: its adapter plus the
// built-in availability probe creation fails closed on. Clients never
// supply an executable or arguments — the daemon runs only these.
type adapterEntry struct {
	adapter harness.Adapter
	// available reports whether the harness can be executed on this
	// machine (production: the installed CLI resolves).
	available func() error
}

// register adds a built-in harness adapter to the daemon.
func (d *Daemon) register(a harness.Adapter, available func() error) {
	d.adapters[a.Name()] = adapterEntry{
		adapter:   a,
		available: available,
	}
}

// adapterFor resolves the adapter owning a durable session's harness.
func (d *Daemon) adapterFor(name string) (harness.Adapter, bool) {
	e, ok := d.adapters[name]
	if !ok {
		return nil, false
	}
	return e.adapter, true
}

// entryFor resolves the registry entry for a harness name.
func (d *Daemon) entryFor(name string) (adapterEntry, bool) {
	e, ok := d.adapters[name]
	return e, ok
}

// quiesceAdapters drains adapter-owned goroutines (transport readers,
// turn-completion workers) that may still be finishing durable writes once
// every runtime process is reaped. Adapters that own such goroutines
// expose a drain seam; harnesses without one (fixture) have nothing to
// drain.
func (d *Daemon) quiesceAdapters() {
	for _, e := range d.adapters {
		if q, ok := e.adapter.(interface{ WaitQuiescent() }); ok {
			q.WaitQuiescent()
		}
	}
}

// onRuntimeGone fans the supervisor's runtime-removal notification out to
// every adapter. Each adapter ignores runtime keys it does not own, and a
// runtime key belongs to exactly one harness family.
func (d *Daemon) onRuntimeGone(key string, rt *runtime.Runtime, reason string) {
	for _, e := range d.adapters {
		e.adapter.OnRuntimeGone(key, rt, reason)
	}
}

// harnessSession resolves a key to a live session plus the adapter for its
// durable harness. A session whose harness has no registered adapter is
// never silently served by another harness.
func (d *Daemon) harnessSession(w http.ResponseWriter, key string) (*session.Managed, harness.Adapter, bool) {
	if !session.ValidKey(key) {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidSessionKey, "invalid session key "+key)
		return nil, nil, false
	}
	m, ok := d.registry.Get(key)
	if !ok {
		writeErr(w, http.StatusNotFound, api.ErrSessionNotFound, "no session "+key)
		return nil, nil, false
	}
	adapter, ok := d.adapterFor(m.Snapshot().Harness)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable,
			"no adapter for harness "+m.Snapshot().Harness)
		return nil, nil, false
	}
	return m, adapter, true
}

// writeAdapterErr maps canonical adapter errors to stable wire codes. A
// busy session or a missing turn is a conflict; a lost native session or a
// dead runtime is a conflict too — the caller must re-establish context. A
// capability mismatch is a stable UNSUPPORTED_OPERATION, never INTERNAL.
func writeAdapterErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, harness.ErrBusy):
		writeErr(w, http.StatusConflict, api.ErrSessionBusy, err.Error())
	case errors.Is(err, harness.ErrNoActiveTurn):
		writeErr(w, http.StatusConflict, api.ErrNoActiveTurn, err.Error())
	case errors.Is(err, harness.ErrNativeSessionLost):
		writeErr(w, http.StatusConflict, api.ErrNativeSessionLost, err.Error())
	case errors.Is(err, harness.ErrNoSuchInput):
		writeErr(w, http.StatusNotFound, api.ErrUnknownInput, err.Error())
	case errors.Is(err, harness.ErrRuntimeUnavailable):
		writeErr(w, http.StatusServiceUnavailable, api.ErrRuntimeUnavailable, err.Error())
	case errors.Is(err, harness.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, api.ErrUnsupportedOperation, err.Error())
	case errors.Is(err, harness.ErrInvalidConfig):
		writeErr(w, http.StatusBadRequest, api.ErrInvalidConfig, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, api.ErrRuntimeUnavailable, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
	}
}
