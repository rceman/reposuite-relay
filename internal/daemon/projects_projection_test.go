// Session project projection: derived projectId/projectName on the wire
// from the catalog + session cwd — longest root wins, renames and root
// edits re-derive immediately, deletes leave sessions Ungrouped.
package daemon

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/rceman/reposuite-relay/internal/session"
)

func TestProjectSessionProjection(t *testing.T) {
	d, _, c, _ := webDaemonBare(t)
	endpoint := d.endpoint()
	setupAdmin(t, d, c, "admin", "correct-horse-12")
	csrf := webCSRF(t, d, c)

	work := t.TempDir()
	inner := work + "/inner"
	deep := inner + "/deep"
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	outer := createCold(t, c, endpoint, csrf, "outer", work)
	innerS := createCold(t, c, endpoint, csrf, "inners", inner)
	deepS := createCold(t, c, endpoint, csrf, "deeps", deep)
	lone := createCold(t, c, endpoint, csrf, "lone", "/nonexistent/unmatched")

	resp := cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, fmt.Sprintf(`{"name":"Work","root":%q}`, work))
	workPrj := decodeProject(t, resp)
	resp = cookieJSON(t, c, http.MethodPost, endpoint, "/v1/projects",
		csrf, fmt.Sprintf(`{"name":"Inner","root":%q}`, inner))
	innerPrj := decodeProject(t, resp)

	want := map[string]struct{ id, name string }{
		outer.Key:  {workPrj.Project.ID, "Work"},
		innerS.Key: {innerPrj.Project.ID, "Inner"},
		deepS.Key:  {innerPrj.Project.ID, "Inner"}, // longest root wins
	}
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions", "", "")
	list := decodeSessions(t, resp)
	for _, s := range list.Sessions {
		if s.Key == lone.Key {
			if s.ProjectID != "" || s.ProjectName != "" {
				t.Fatalf("unmatched session %s projected %+v", s.Key, s)
			}
			continue
		}
		w, ok := want[s.Key]
		if !ok {
			continue
		}
		if s.ProjectID != w.id || s.ProjectName != w.name {
			t.Fatalf("%s projected %q/%q, want %q/%q", s.Key, s.ProjectID, s.ProjectName, w.id, w.name)
		}
	}
	// Detail read carries the same projection.
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/"+deepS.Key, "", "")
	det := decodeSessionResp(t, resp)
	if det.Session.ProjectID != innerPrj.Project.ID {
		t.Fatalf("detail projection = %q, want %q", det.Session.ProjectID, innerPrj.Project.ID)
	}
	// Rename: same ID, new name — immediately visible.
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/"+workPrj.Project.ID,
		csrf, `{"name":"Renamed Work"}`)
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/"+outer.Key, "", "")
	det = decodeSessionResp(t, resp)
	if det.Session.ProjectID != workPrj.Project.ID || det.Session.ProjectName != "Renamed Work" {
		t.Fatalf("rename projection = %+v", det.Session)
	}
	// Root edit re-derives membership immediately: moving Inner's root to
	// `deep` leaves `inner` matching only Work.
	resp = cookieJSON(t, c, http.MethodPatch, endpoint, "/v1/projects/"+innerPrj.Project.ID,
		csrf, fmt.Sprintf(`{"root":%q}`, deep))
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/"+innerS.Key, "", "")
	det = decodeSessionResp(t, resp)
	if det.Session.ProjectID != workPrj.Project.ID {
		t.Fatalf("root-move projection = %q, want Work %q", det.Session.ProjectID, workPrj.Project.ID)
	}
	// Delete Work: outer/inner fall to Inner? No — inner's root moved to
	// deep; `outer` (cwd=work) and `inner` (cwd=inner) become Ungrouped,
	// `deep` still matches the moved Inner project.
	resp = cookieJSON(t, c, http.MethodDelete, endpoint, "/v1/projects/"+workPrj.Project.ID, csrf, "")
	resp.Body.Close()
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions", "", "")
	list = decodeSessions(t, resp)
	for _, s := range list.Sessions {
		switch s.Key {
		case deepS.Key:
			if s.ProjectID != innerPrj.Project.ID {
				t.Fatalf("deep session lost its moved project: %+v", s)
			}
		case outer.Key, innerS.Key, lone.Key:
			if s.ProjectID != "" {
				t.Fatalf("session %s should be Ungrouped: %+v", s.Key, s)
			}
		}
	}
	// Sessions still exist and stay COLD after all project mutations.
	resp = cookieJSON(t, c, http.MethodGet, endpoint, "/v1/sessions/"+outer.Key, "", "")
	det = decodeSessionResp(t, resp)
	if det.Session.RuntimeState != session.RuntimeCold || det.Session.PID != 0 ||
		det.Session.Generation != 0 {
		t.Fatalf("project CRUD must never wake a session: %+v", det.Session)
	}
}
