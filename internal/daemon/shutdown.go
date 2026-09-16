package daemon

import (
	"net"
	"net/http"
)

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
