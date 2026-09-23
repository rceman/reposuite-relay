// Relay-local project catalog over the /v1 surface: CRUD, auth/CSRF
// domains, session projection, COLD guarantee, and restart persistence.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/adminauth"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/presentation"
	"github.com/rceman/reposuite-relay/internal/session"
)

// webDaemonBare starts a daemon (no harness fakes needed — project tests
// use only COLD fixture-free durable sessions created through the API)
// with a cookie-jar browser client. The returned channel is Serve's
// completion — restart tests wait on it before starting the next
// generation.
func webDaemonBare(t *testing.T) (*Daemon, paths.Paths, *http.Client, chan struct{}) {
	t.Helper()
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := serveDaemon(t, d)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, p, &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, done
}

func decodeProjects(t *testing.T, resp *http.Response) api.ProjectList {
	t.Helper()
	defer resp.Body.Close()
	var out api.ProjectList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode project list: %v", err)
	}
	return out
}

func decodeProject(t *testing.T, resp *http.Response) api.ProjectResponse {
	t.Helper()
	defer resp.Body.Close()
	var out api.ProjectResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode project response: %v", err)
	}
	return out
}

func decodeErrBody(t *testing.T, resp *http.Response) api.ErrorBody {
	t.Helper()
	defer resp.Body.Close()
	var out api.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out
}

// createCold makes a durable COLD session through the cookie domain.
func createCold(t *testing.T, c *http.Client, endpoint, csrf, key, cwd string) api.SessionInfo {
	t.Helper()
	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/sessions/codex",
		csrf, fmt.Sprintf(`{"key":%q,"cwd":%q}`, key, cwd))
	got := decodeSessionResp(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create %s: %d", key, resp.StatusCode)
	}
	if s := got.Session; s.RuntimeState != session.RuntimeCold || s.PID != 0 || s.Generation != 0 {
		t.Fatalf("create must stay COLD: %+v", s)
	}
	return got.Session
}

func TestProjectAPIBrowserDomain(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	// Empty catalog initially.
	resp := cookieJSON(t, c, http.MethodGet, endpoint, "/v1/projects", "", "")
	list := decodeProjects(t, resp)
	if resp.StatusCode != http.StatusOK || len(list.Projects) != 0 {
		t.Fatalf("initial projects: %d %+v", resp.StatusCode, list.Projects)
	}
	// Anonymous and CSRF-less unsafe calls fail; bearer does not.
	anon, err := (&http.Client{Timeout: 5 * time.Second}).Get(endpoint + "/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET projects: %d, want 401", anon.StatusCode)
	}
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		"", `{"name":"Relay","root":"/work/relay"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF: %d, want 403", resp.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/projects",
		strings.NewReader(`{"name":"Bearer-made","root":"/work/bearer"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.Token())
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	created := decodeProject(t, resp)
	if resp.StatusCode != http.StatusOK || !presentation.ValidID(created.Project.ID) {
		t.Fatalf("bearer POST without CSRF: %d %+v", resp.StatusCode, created)
	}
	// CRUD through the cookie domain.
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, `{"name":" Relay ","root":"/work/relay/"}`)
	made := decodeProject(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if made.Project.Name != "Relay" || made.Project.Root != "/work/relay" {
		t.Fatalf("create must normalize name/root: %+v", made.Project)
	}
	if !presentation.ValidID(made.Project.ID) {
		t.Fatalf("non-canonical id %q", made.Project.ID)
	}
	// Duplicate canonical root → 409 PROJECT_ROOT_CONFLICT.
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, `{"name":"dup","root":"/work/relay"}`)
	env := decodeErrBody(t, resp)
	if resp.StatusCode != http.StatusConflict || env.Error.Code != api.ErrProjectRootConflict {
		t.Fatalf("duplicate root: %d %v", resp.StatusCode, env.Error)
	}
	// PATCH renames; same ID.
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/"+made.Project.ID,
		csrf, `{"name":"RepoSuite Relay"}`)
	upd := decodeProject(t, resp)
	if resp.StatusCode != http.StatusOK || upd.Project.Name != "RepoSuite Relay" ||
		upd.Project.ID != made.Project.ID {
		t.Fatalf("patch: %d %+v", resp.StatusCode, upd.Project)
	}
	// Unknown id → 404 PROJECT_NOT_FOUND.
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/prj_"+
		strings.Repeat("0", 32), csrf, `{"name":"x"}`)
	env = decodeErrBody(t, resp)
	if resp.StatusCode != http.StatusNotFound || env.Error.Code != api.ErrProjectNotFound {
		t.Fatalf("patch unknown: %d %v", resp.StatusCode, env.Error)
	}
	resp = cookieJSON(t, c, http.MethodDelete, endpoint, "/v1/projects/"+made.Project.ID, csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/projects", "", "")
	list = decodeProjects(t, resp)
	if len(list.Projects) != 1 || list.Projects[0].Name != "Bearer-made" {
		t.Fatalf("final list = %+v, want only Bearer-made", list.Projects)
	}
}

func decodeSessions(t *testing.T, resp *http.Response) api.SessionList {
	t.Helper()
	defer resp.Body.Close()
	var out api.SessionList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	return out
}

// TestProjectPostCommitHTTP: locks the actual API semantic the Web UI
// must accommodate — a mutation whose rename committed but whose
// directory fsync failed returns 500 INTERNAL while the canonical
// catalog already contains the change. A rejected mutation is NOT a
// rollback.
func TestProjectPostCommitHTTP(t *testing.T) {
	injected := errors.New("injected dirsync failure")
	p := testPaths(t)
	d, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
		PresentationHooks: &presentation.Hooks{
			SyncDir: func(dir string) error { return injected },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	// Create: rename commits, dirsync fails → 500 INTERNAL, but the
	// project IS in the canonical catalog.
	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, `{"name":"Committed","root":"/work/committed"}`)
	env := decodeErrBody(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || env.Error.Code != api.ErrInternal {
		t.Fatalf("post-commit create: %d %v", resp.StatusCode, env.Error)
	}
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/projects", "", "")
	list := decodeProjects(t, resp)
	if len(list.Projects) != 1 || list.Projects[0].Name != "Committed" {
		t.Fatalf("committed catalog after 500 = %+v — the mutation DID commit", list.Projects)
	}
	// Delete the committed project: same post-commit semantics —
	// 500 INTERNAL, and the catalog visibly no longer contains it.
	resp = cookieJSON(t, c, http.MethodDelete, endpoint,
		"/v1/projects/"+list.Projects[0].ID, csrf, "")
	env = decodeErrBody(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || env.Error.Code != api.ErrInternal {
		t.Fatalf("post-commit delete: %d %v", resp.StatusCode, env.Error)
	}
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/projects", "", "")
	list = decodeProjects(t, resp)
	if len(list.Projects) != 0 {
		t.Fatalf("catalog after post-commit delete = %+v, want empty", list.Projects)
	}
}
