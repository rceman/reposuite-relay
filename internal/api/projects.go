package api

// Relay-local project DTOs — the /v1/projects surface. A Project is
// presentation/grouping metadata only: it carries no session lifecycle
// or workflow authority, and membership is derived from session cwd at
// read time, never persisted on the session.

// ProjectInfo is the wire view of a Relay-local project: a stable
// opaque ID, a display name, and an absolute filesystem grouping root.
type ProjectInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

// ProjectList is GET /v1/projects: the catalog in canonical order
// (name, root, id).
type ProjectList struct {
	Daemon   DaemonInfo    `json:"daemon"`
	Projects []ProjectInfo `json:"projects"`
}

// CreateProjectRequest is POST /v1/projects.
type CreateProjectRequest struct {
	Name string `json:"name"`
	Root string `json:"root"`
}

// UpdateProjectRequest is PATCH /v1/projects/{id} — only the fields to
// change are sent; the ID is immutable.
type UpdateProjectRequest struct {
	Name *string `json:"name,omitempty"`
	Root *string `json:"root,omitempty"`
}

// ProjectResponse is the mutating single-project response.
type ProjectResponse struct {
	Daemon  DaemonInfo  `json:"daemon"`
	Project ProjectInfo `json:"project"`
}
