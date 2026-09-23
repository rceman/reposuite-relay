// Machine API token management — admin-only Settings surface. The
// persistent machine bearer is NEVER readable through any ordinary API;
// the only channel that returns a credential is the explicit rotation
// operation, and only the newly generated value. Bearer credential
// domains (descriptor, machine) cannot reach these routes at all.
package daemon

import (
	"net/http"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/auth"
)

// handleMachineTokenStatus reports metadata only — presence, never the
// credential itself, never a path or prefix.
func (d *Daemon) handleMachineTokenStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, api.MachineTokenStatus{
		Daemon:     d.info(),
		Configured: true, // Ensure() ran at startup or Start failed closed
	})
}

// handleRotateMachineToken mints a fresh 256-bit bearer, commits it
// atomically over config/api.token, and converges live authentication.
// The rotation holds the write lock across generate+commit+converge so
// concurrent rotations serialize and no request observes a torn
// credential.
//
// Commit point: the atomic rename inside auth.Rotate. A pre-commit
// error leaves the old token canonical everywhere and returns no new
// token. A post-commit dirsync failure still commits the credential —
// the response returns it with DurabilityConfirmed=false.
func (d *Daemon) handleRotateMachineToken(w http.ResponseWriter, _ *http.Request, _ string) {
	// The rotation response carries a credential — never cacheable,
	// body-only (never URL/query/cookie).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	tok, err := auth.Rotate(d.paths.MachineToken(), d.opts.MachineTokenHooks)
	switch {
	case err == nil:
		d.machineToken = tok
		writeJSON(w, http.StatusOK, api.MachineTokenRotateResponse{
			Daemon:              d.info(),
			Token:               tok,
			DurabilityConfirmed: true,
		})
	case auth.IsPostCommit(err):
		// Committed but durability unconfirmed: converge live authority
		// and return the token — never pretend the rotation rolled back.
		d.machineToken = tok
		writeJSON(w, http.StatusOK, api.MachineTokenRotateResponse{
			Daemon:              d.info(),
			Token:               tok,
			DurabilityConfirmed: false,
		})
	default:
		// Pre-commit: the old credential remains canonical on disk and
		// in memory; no token material leaves this handler.
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, "machine token rotation failed")
	}
}
