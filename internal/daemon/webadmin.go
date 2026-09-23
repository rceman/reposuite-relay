// Web Admin authentication (ADR-006 §Web Admin): the browser credential
// domain behind the embedded SvelteKit SPA. Three credential domains
// exist: the ephemeral descriptor bearer, the persistent machine bearer,
// and the admin cookie session. Cookie authentication is browser-only
// and carries CSRF obligations that the bearer domains do not.
package daemon

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// adminCookieName is the browser session cookie. It is deliberately NOT
// Secure: the canonical transport is http://127.0.0.1:<port> — Secure
// would make the cookie unusable over the accepted loopback contract.
const adminCookieName = "relay_admin"

// adminCSRFField is the form field carrying the per-session CSRF token.
const adminCSRFField = "csrf"

// maxAuthFormBytes bounds an auth form body.
const maxAuthFormBytes = 8 << 10

// webAuth is the daemon's live browser-auth state. creds transitions
// once — nil → the committed credential — under webMu, and is read
// atomically everywhere else.
type webAuth struct {
	creds    atomic.Pointer[adminauth.Credentials] // nil = setup mode
	mgr      *adminauth.Manager
	kdf      adminauth.Params
	origin   string // exact canonical origin http://127.0.0.1:<port>
	hostPort string // exact accepted Host header

	// Test seams — production defaults apply when nil.
	verify func(password, encoded string) bool
	hooks  *adminauth.Hooks
}

// authKind distinguishes the credential domain that authenticated a
// request — cookie auth is the only domain that carries browser CSRF
// obligations.
type authKind int

const (
	authNone authKind = iota
	authDescriptorBearer
	authMachineBearer
	authAdminCookie
)

// newWebAuth loads the admin credential (absent = setup mode) and builds
// the runtime browser-auth state. Malformed or insecure admin.json fails
// startup closed.
func newWebAuth(p paths.Paths, endpoint string, opts Options) (*webAuth, error) {
	mgr, err := adminauth.NewManager(opts.AdminClock)
	if err != nil {
		return nil, err
	}
	kdf := opts.AdminKDF
	if kdf == (adminauth.Params{}) {
		kdf = adminauth.ProductionParams
	}
	if err := kdf.Validate(); err != nil {
		return nil, fmt.Errorf("admin KDF params: %w", err)
	}
	w := &webAuth{
		mgr:    mgr,
		kdf:    kdf,
		origin: endpoint,
		hooks:  opts.AdminHooks,
		verify: adminauth.Verify,
	}
	if opts.AdminVerify != nil {
		w.verify = opts.AdminVerify
	}
	_, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "http://"))
	if err != nil {
		return nil, fmt.Errorf("admin auth endpoint: %w", err)
	}
	w.hostPort = config.ListenHost + ":" + port
	creds, err := adminauth.Load(p.AdminCredentials())
	switch {
	case err == nil:
		w.creds.Store(&creds)
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("admin credentials: %w", err)
	}
	return w, nil
}

// configured reports whether an admin credential exists.
func (w *webAuth) configured() bool { return w.creds.Load() != nil }

// credentials returns the loaded credential (nil in setup mode).
func (w *webAuth) credentials() *adminauth.Credentials { return w.creds.Load() }

// hostOK enforces exact Host: only 127.0.0.1:<configured-port> is the
// Web Admin authority — DNS-rebinding attempts through any other name
// fail before routing. X-Forwarded-Host is never consulted.
func (w *webAuth) hostOK(r *http.Request) bool {
	return r.Host == w.hostPort
}

// originOK enforces the exact canonical Origin for browser-auth unsafe
// operations.
func (w *webAuth) originOK(r *http.Request) bool {
	return r.Header.Get("Origin") == w.origin
}

// securityHeaders applies the restrictive Web Admin policy: no sniffing,
// no framing, no referrer, no cache, no scripts/objects beyond self.
func (w *webAuth) securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy",
		"default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

// cookieSession resolves the admin cookie to a live browser session.
func (w *webAuth) cookieSession(r *http.Request) (*adminauth.Session, string, bool) {
	c, err := r.Cookie(adminCookieName)
	if err != nil || c.Value == "" {
		return nil, "", false
	}
	s, ok := w.mgr.Lookup(c.Value)
	return s, c.Value, ok
}

// authenticate classifies the request's credential: descriptor bearer,
// machine bearer, or admin cookie. Cookie auth is only accepted on the
// canonical Host so a DNS-rebind page can never ride it.
func (d *Daemon) authenticate(r *http.Request) (authKind, *adminauth.Session) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		got := []byte(h[len(prefix):])
		if subtle.ConstantTimeCompare(got, []byte(d.token)) == 1 {
			return authDescriptorBearer, nil
		}
		if d.machineBearerOK(got) {
			return authMachineBearer, nil
		}
	}
	if s, _, ok := d.web.cookieSession(r); ok {
		return authAdminCookie, s
	}
	return authNone, nil
}

// machineBearerOK linearizes the machine-credential admission decision:
// the ENTIRE constant-time compare runs inside the read lock — never a
// copied-out credential compared after release. A compare that completes
// before a rotation's write-locked commit authenticates the old
// credential (the request finishes normally); every compare that begins
// after the rotation's lock release sees only the committed token.
// MachineAuthGate is a test-only seam inside the critical section.
func (d *Daemon) machineBearerOK(got []byte) bool {
	d.machineMu.RLock()
	defer d.machineMu.RUnlock()
	if d.opts.MachineAuthGate != nil {
		d.opts.MachineAuthGate()
	}
	return subtle.ConstantTimeCompare(got, []byte(d.machineToken)) == 1
}

// setSessionCookie issues the authenticated browser session cookie.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie expires the admin cookie deterministically.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
