package daemon

import (
	"net/http"
	"strings"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
)

// route dispatches one request. The canonical Host is enforced before
// any routing: only 127.0.0.1:<configured-port> is this Relay — a
// foreign Host (DNS rebinding, forwarded names) fails closed. The Web
// Admin surface lives at / and /auth/*; every /v1 route requires one of
// the three credential domains: descriptor bearer, machine bearer, or
// an authenticated admin browser session.
func (d *Daemon) route(w http.ResponseWriter, r *http.Request) {
	if !d.web.hostOK(r) {
		writeErr(w, http.StatusMisdirectedRequest, api.ErrInvalidRequest, "unexpected host")
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/auth/"):
		// Web Admin auth endpoints — Go-owned, never the SPA.
		d.routeWeb(w, r)
		return
	case p == "/v1" || strings.HasPrefix(p, "/v1/"):
		// Machine API — credential domains enforced below.
	default:
		// Everything else is the embedded Web Admin surface: static
		// assets or the SPA fallback document.
		d.serveStatic(w, r)
		return
	}
	kind, sess := d.authenticate(r)
	if kind == authNone {
		writeErr(w, http.StatusUnauthorized, api.ErrUnauthorized, "missing or invalid credentials")
		return
	}
	// Cookie-authenticated unsafe requests carry browser CSRF
	// obligations: exact canonical Origin plus the session's CSRF token
	// in X-Relay-CSRF. Bearer clients are unaffected.
	if kind == authAdminCookie && unsafeMethod(r.Method) {
		if !d.web.originOK(r) || !d.web.mgr.CheckCSRF(sess, r.Header.Get("X-Relay-CSRF")) {
			writeErr(w, http.StatusForbidden, api.ErrUnauthorized, "browser CSRF check failed")
			return
		}
	}
	switch {
	case p == "/v1/daemon" && r.Method == http.MethodGet:
		d.handleDaemonInfo(w, r)
	case p == "/v1/daemon/shutdown" && r.Method == http.MethodPost:
		d.handleShutdown(w, r)
	case p == "/v1/projects" && r.Method == http.MethodGet:
		d.handleListProjects(w, r)
	case p == "/v1/projects" && r.Method == http.MethodPost:
		d.lifecycle(d.handleCreateProject)(w, r, "")
	case strings.HasPrefix(p, "/v1/projects/"):
		d.routeProject(w, r, strings.TrimPrefix(p, "/v1/projects/"))
	case p == "/v1/sessions" && r.Method == http.MethodGet:
		d.handleList(w, r)
	case p == "/v1/sessions/fixture" && r.Method == http.MethodPost:
		d.lifecycle(d.handleServeFixture)(w, r, "")
	case p == "/v1/sessions/codex" && r.Method == http.MethodPost:
		d.lifecycle(d.handleCreateHarness)(w, r, session.HarnessCodex)
	case p == "/v1/sessions/devin" && r.Method == http.MethodPost:
		d.lifecycle(d.handleCreateHarness)(w, r, session.HarnessDevin)
	case p == "/v1/sessions/opencode" && r.Method == http.MethodPost:
		d.lifecycle(d.handleCreateHarness)(w, r, session.HarnessOpenCode)
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
	case strings.HasSuffix(rest, "/prompt"):
		key := strings.TrimSuffix(rest, "/prompt")
		if strings.Contains(key, "/") || r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycle(d.handlePrompt)(w, r, key)
	case strings.HasSuffix(rest, "/cancel"):
		key := strings.TrimSuffix(rest, "/cancel")
		if strings.Contains(key, "/") || r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycle(d.handleCancel)(w, r, key)
	case strings.HasSuffix(rest, "/input"):
		key := strings.TrimSuffix(rest, "/input")
		if strings.Contains(key, "/") || r.Method != http.MethodPost {
			methodOrNotFound(w, r, http.MethodPost)
			return
		}
		d.lifecycle(d.handleInput)(w, r, key)
	case strings.HasSuffix(rest, "/config"):
		key := strings.TrimSuffix(rest, "/config")
		if strings.Contains(key, "/") || r.Method != http.MethodPatch {
			methodOrNotFound(w, r, http.MethodPatch)
			return
		}
		d.lifecycle(d.handleConfig)(w, r, key)
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

// unsafeMethod reports whether the method mutates state — cookie-auth
// requests with these methods carry the browser CSRF obligation.
func unsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
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
