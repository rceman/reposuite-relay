// Web Admin request handlers: the root shell plus the /auth/* surface.
// Setup and login are pre-auth endpoints guarded by exact Origin plus a
// per-daemon HMAC form token; authenticated mutations carry the session
// CSRF token.
package daemon

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
)

// routeWeb handles the Web Admin surface. The caller has already
// enforced the canonical Host header.
func (d *Daemon) routeWeb(w http.ResponseWriter, r *http.Request) {
	d.web.securityHeaders(w.Header())
	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet {
			methodOrNotFound(w, r, http.MethodGet)
			return
		}
		d.handleWebRoot(w, r)
	case "/auth/setup":
		if r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycleN(d.handleSetup)(w, r)
	case "/auth/login":
		if r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycleN(d.handleLogin)(w, r)
	case "/auth/logout":
		if r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycleN(d.handleLogout)(w, r)
	case "/auth/session":
		if r.Method != http.MethodGet {
			methodOrNotFound(w, r, http.MethodGet)
			return
		}
		d.handleAuthSession(w, r)
	default:
		writeErr(w, http.StatusNotFound, api.ErrInvalidRequest, "unknown route")
	}
}

// lifecycleN is the shutdown barrier for keyless mutating web handlers:
// once shutdown begins, no new setup/login/logout is admitted.
func (d *Daemon) lifecycleN(h func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.shutting {
			d.mu.Unlock()
			writeErr(w, http.StatusServiceUnavailable, api.ErrShuttingDown, "daemon is shutting down")
			return
		}
		d.wg.Add(1)
		d.mu.Unlock()
		defer d.wg.Done()
		h(w, r)
	}
}

// webPage is the template view model.
type webPage struct {
	Mode      string // setup | login | shell
	Username  string
	FormToken string
	CSRFToken string
	Error     string
}

// handleWebRoot renders the state-dependent shell: setup when no admin
// exists, login when unauthenticated, the minimal shell when signed in.
func (d *Daemon) handleWebRoot(w http.ResponseWriter, r *http.Request) {
	if !d.web.configured() {
		d.renderWeb(w, webPage{
			Mode:      "setup",
			FormToken: d.web.mgr.FormToken("setup"),
		})
		return
	}
	if s, _, ok := d.web.cookieSession(r); ok {
		d.renderWeb(w, webPage{
			Mode:      "shell",
			Username:  s.Username,
			CSRFToken: s.CSRFToken,
		})
		return
	}
	d.renderWeb(w, webPage{
		Mode:      "login",
		FormToken: d.web.mgr.FormToken("login"),
	})
}

// formDecode bounds and decodes one urlencoded form body.
func formDecode(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthFormBytes)
	return r.ParseForm()
}

// handleSetup performs first-run admin setup: exact Origin + form token
// (pre-auth CSRF), validated inputs, Argon2id hash, create-once commit,
// then an authenticated browser session.
func (d *Daemon) handleSetup(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		writeErr(w, status, api.ErrInvalidRequest, msg)
	}
	if !d.web.originOK(r) {
		fail(http.StatusForbidden, "origin mismatch")
		return
	}
	if err := formDecode(w, r); err != nil {
		fail(http.StatusBadRequest, "invalid form")
		return
	}
	if !d.web.mgr.CheckFormToken("setup", r.FormValue("form_token")) {
		fail(http.StatusForbidden, "invalid form token")
		return
	}
	if d.web.configured() {
		fail(http.StatusConflict, "admin already configured")
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if r.FormValue("confirm") != password {
		fail(http.StatusBadRequest, "passwords do not match")
		return
	}
	if err := adminauth.ValidateUsername(username); err != nil {
		fail(http.StatusBadRequest, "invalid username")
		return
	}
	if err := adminauth.ValidatePassword(password); err != nil {
		fail(http.StatusBadRequest, "invalid password")
		return
	}
	hash, err := adminauth.Hash(password, d.web.kdf)
	if err != nil {
		fail(http.StatusInternalServerError, "credential hashing failed")
		return
	}
	// Commit is serialized: exactly one concurrent setup wins; the file
	// itself is created atomically without ever overwriting.
	d.webMu.Lock()
	defer d.webMu.Unlock()
	if d.web.configured() {
		fail(http.StatusConflict, "admin already configured")
		return
	}
	creds := adminauth.Credentials{
		SchemaVersion: adminauth.SchemaVersion,
		Username:      username,
		PasswordHash:  hash,
	}
	created, err := adminauth.CommitOnceWith(d.paths.AdminCredentials(), creds, d.web.hooks)
	if err != nil {
		var pce *adminauth.PostCommitError
		if errors.As(err, &pce) {
			// The credential IS committed — never report a rollback and
			// never leave the daemon in setup mode where a second attempt
			// could redefine authority. Publish the committed credential,
			// fail closed with a bounded diagnostic, and restart so an
			// operator reboot reconciles the durable state from disk.
			d.web.creds.Store(&creds)
			writeErr(w, http.StatusInternalServerError, api.ErrInternal,
				"admin credential committed but post-commit durability failed; restarting")
			go d.initiate()
			return
		}
		fail(http.StatusInternalServerError, "admin credential commit failed")
		return
	}
	if !created {
		fail(http.StatusConflict, "admin already configured")
		return
	}
	d.web.creds.Store(&creds)
	token, _, err := d.web.mgr.CreateSession(username)
	if err != nil {
		fail(http.StatusInternalServerError, "session creation failed")
		return
	}
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogin verifies credentials and issues a browser session. Wrong
// username and wrong password share one public failure shape, and every
// admitted attempt performs exactly one Argon2id verification against
// the stored credential — username correctness never short-circuits it.
func (d *Daemon) handleLogin(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		writeErr(w, status, api.ErrInvalidRequest, msg)
	}
	if !d.web.originOK(r) {
		fail(http.StatusForbidden, "origin mismatch")
		return
	}
	if err := formDecode(w, r); err != nil {
		fail(http.StatusBadRequest, "invalid form")
		return
	}
	if !d.web.mgr.CheckFormToken("login", r.FormValue("form_token")) {
		fail(http.StatusForbidden, "invalid form token")
		return
	}
	if !d.web.configured() {
		fail(http.StatusConflict, "admin is not configured")
		return
	}
	if th := d.web.mgr.CheckLoginThrottle(); !th.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(th.RetryAfter.Round(time.Second).Seconds())+1))
		fail(http.StatusTooManyRequests, "too many login attempts")
		return
	}
	// Exactly one Argon2id verification per admitted attempt, always
	// against the same stored credential — with a single admin account a
	// dummy hash is unnecessary, and skipping Verify on a wrong username
	// would make the attempt trivially distinguishable.
	creds := d.web.credentials()
	usernameOK := subtle.ConstantTimeCompare(
		[]byte(r.FormValue("username")), []byte(creds.Username)) == 1
	passwordOK := d.web.verify(r.FormValue("password"), creds.PasswordHash)
	if !(usernameOK && passwordOK) {
		d.web.mgr.RecordLoginFailure()
		fail(http.StatusUnauthorized, "invalid credentials")
		return
	}
	d.web.mgr.RecordLoginSuccess()
	token, _, err := d.web.mgr.CreateSession(creds.Username)
	if err != nil {
		fail(http.StatusInternalServerError, "session creation failed")
		return
	}
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout revokes the browser session: exact Origin + the session
// CSRF token posted as a form field.
func (d *Daemon) handleLogout(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		writeErr(w, status, api.ErrInvalidRequest, msg)
	}
	if !d.web.originOK(r) {
		fail(http.StatusForbidden, "origin mismatch")
		return
	}
	s, cookie, ok := d.web.cookieSession(r)
	if !ok {
		fail(http.StatusUnauthorized, "not authenticated")
		return
	}
	if err := formDecode(w, r); err != nil {
		fail(http.StatusBadRequest, "invalid form")
		return
	}
	if !d.web.mgr.CheckCSRF(s, r.FormValue(adminCSRFField)) {
		fail(http.StatusForbidden, "invalid CSRF token")
		return
	}
	d.web.mgr.Revoke(cookie)
	clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleAuthSession reports the browser-auth projection. It never
// returns the password hash, the machine token, the descriptor bearer,
// or the browser session token itself — only the session CSRF token.
func (d *Daemon) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"configured":    d.web.configured(),
		"authenticated": false,
	}
	if s, _, ok := d.web.cookieSession(r); ok && d.web.configured() {
		out["authenticated"] = true
		out["username"] = s.Username
		out["csrfToken"] = s.CSRFToken
	}
	writeJSON(w, http.StatusOK, out)
}

// renderWeb renders the shell template.
func (d *Daemon) renderWeb(w http.ResponseWriter, page webPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webTemplate.Execute(w, page); err != nil {
		fmt.Fprintf(os.Stderr, "relayd: web template: %v\n", err)
	}
}
