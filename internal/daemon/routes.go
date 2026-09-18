package daemon

import (
	"crypto/subtle"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/session"
	"net/http"
	"strings"
)

// route dispatches one request. Every route requires the bearer token,
// including daemon status, transcript, the event stream, and shutdown.
func (d *Daemon) route(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		writeErr(w, http.StatusUnauthorized, api.ErrUnauthorized, "missing or invalid bearer token")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/v1/daemon" && r.Method == http.MethodGet:
		d.handleDaemonInfo(w, r)
	case p == "/v1/daemon/shutdown" && r.Method == http.MethodPost:
		d.handleShutdown(w, r)
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

// authorized performs constant-time bearer token comparison.
func (d *Daemon) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := h[len(prefix):]
	if len(got) != len(d.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.token)) == 1
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
