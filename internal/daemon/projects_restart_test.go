// Session-store isolation and restart persistence for the project
// catalog: grouping metadata must never touch durable session.json, and
// the catalog reloads identically across daemon generations.
package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rceman/reposuite-relay/internal/adminauth"
)

// TestProjectNeverTouchesSessionJSON: session.json is byte-identical
// across create/rename/move/delete — grouping mutates only
// presentation.json.
func TestProjectNeverTouchesSessionJSON(t *testing.T) {
	d, p, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	work := t.TempDir()
	s := createCold(t, c, endpoint, csrf, "immut", work)
	metaPath := filepath.Join(p.SessionsDir(), s.SessionID, "session.json")
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, fmt.Sprintf(`{"name":"W","root":%q}`, work))
	made := decodeProject(t, resp)
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/"+made.Project.ID,
		csrf, `{"name":"W2"}`)
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/"+made.Project.ID,
		csrf, `{"root":"/elsewhere"}`)
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodDelete, endpoint, "/v1/projects/"+made.Project.ID, csrf, "")
	resp.Body.Close()
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("project CRUD must never mutate durable session.json")
	}
}

// TestProjectRestartPersistence: projects survive a daemon restart with
// identical IDs/roots and the derived grouping is unchanged; malformed
// presentation config fails startup closed.
func TestProjectRestartPersistence(t *testing.T) {
	d, p, c, done := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	work := t.TempDir()
	s := createCold(t, c, endpoint, csrf, "persisted", work+"/sub")
	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, fmt.Sprintf(`{"name":"Stable","root":%q}`, work))
	made := decodeProject(t, resp)
	stopDaemonNow(t, d, done)

	d2, err := Start(p, Options{
		SelfExe:  testBinary(),
		AdminKDF: adminauth.TestParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	// Bearer auth suffices for the read-side assertions — the catalog and
	// the projection are identical across credential domains.
	resp = rawGet(t, d2.endpoint(), "/v1/projects", d2.Token())
	list := decodeProjects(t, resp)
	if len(list.Projects) != 1 || list.Projects[0].ID != made.Project.ID ||
		list.Projects[0].Root != made.Project.Root {
		t.Fatalf("restored projects = %+v", list.Projects)
	}
	resp = rawGet(t, d2.endpoint(), "/v1/sessions/"+s.Key, d2.Token())
	det := decodeSessionResp(t, resp)
	if det.Session.ProjectID != made.Project.ID || det.Session.ProjectName != "Stable" {
		t.Fatalf("restored projection = %+v", det.Session)
	}
}

// TestPresentationMalformedFailsClosed: a corrupt presentation.json
// fails startup closed — before any descriptor or listener exists.
func TestPresentationMalformedFailsClosed(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PresentationConfig(),
		[]byte(`{"schemaVersion":1,"projects":[{"id":"bogus","name":"x","root":"/a"}]}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
		t.Fatal("malformed presentation config must fail startup closed")
	}
	if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
		t.Fatal("no descriptor may name a generation that failed startup")
	}
	// The malformed file is never rewritten.
	raw, _ := os.ReadFile(p.PresentationConfig())
	if !strings.Contains(string(raw), `"bogus"`) {
		t.Fatal("malformed authority must not be silently rewritten")
	}
}
