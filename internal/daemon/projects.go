package daemon

import (
	"errors"
	"net/http"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/presentation"
)

// --- projects: Relay-local presentation metadata -------------------------
// Projects are grouping metadata only: CRUD mutates presentation.json and
// never a RelaySession, runtime, transcript, or filesystem path. Session
// membership is derived at read time from cwd — see sessionInfo.

func projectDTO(p presentation.Project) api.ProjectInfo {
	return api.ProjectInfo{ID: p.ID, Name: p.Name, Root: p.Root}
}

// projectErr maps catalog errors onto the stable machine-error surface:
// validation is INVALID_REQUEST, a duplicate canonical root is
// PROJECT_ROOT_CONFLICT, an unknown ID is PROJECT_NOT_FOUND, and a
// post-commit durability failure is INTERNAL (the catalog IS committed —
// never reported as a rollback).
func (d *Daemon) projectErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, presentation.ErrNotFound):
		writeErr(w, http.StatusNotFound, api.ErrProjectNotFound, err.Error())
	case errors.Is(err, presentation.ErrRootConflict):
		writeErr(w, http.StatusConflict, api.ErrProjectRootConflict, err.Error())
	case presentation.IsPostCommit(err):
		writeErr(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, err.Error())
	}
}

func (d *Daemon) handleListProjects(w http.ResponseWriter, r *http.Request) {
	resp := api.ProjectList{Daemon: d.info(), Projects: []api.ProjectInfo{}}
	for _, p := range d.pres.Snapshot().Projects {
		resp.Projects = append(resp.Projects, projectDTO(p))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) handleCreateProject(w http.ResponseWriter, r *http.Request, _ string) {
	var req api.CreateProjectRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	prj, err := d.pres.Create(req.Name, req.Root)
	if err != nil {
		d.projectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ProjectResponse{Daemon: d.info(), Project: projectDTO(prj)})
}

// routeProject handles /v1/projects/{id} — PATCH and DELETE only.
func (d *Daemon) routeProject(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || !presentation.ValidID(id) {
		writeErr(w, http.StatusNotFound, api.ErrProjectNotFound, "no project "+id)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		d.lifecycle(d.handleUpdateProject)(w, r, id)
	case http.MethodDelete:
		d.lifecycle(d.handleDeleteProject)(w, r, id)
	default:
		methodOrNotFound(w, r, http.MethodPatch, http.MethodDelete)
	}
}

func (d *Daemon) handleUpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	var req api.UpdateProjectRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == nil && req.Root == nil {
		writeErr(w, http.StatusBadRequest, api.ErrInvalidRequest, "empty project patch")
		return
	}
	var name, root string
	setName, setRoot := req.Name != nil, req.Root != nil
	if setName {
		name = *req.Name
	}
	if setRoot {
		root = *req.Root
	}
	prj, err := d.pres.Update(id, name, root, setName, setRoot)
	if err != nil {
		d.projectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ProjectResponse{Daemon: d.info(), Project: projectDTO(prj)})
}

func (d *Daemon) handleDeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	if err := d.pres.Delete(id); err != nil {
		d.projectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.DaemonResponse{Daemon: d.info()})
}
