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

// --- machine-token management ----------------------------------------
// Admin-only Settings surface. The status response carries metadata
// only — the persistent machine token is never readable through any
// ordinary API. The rotate response is the single channel that returns
// a credential, and only the freshly generated one.

// MachineTokenStatus is GET /v1/settings/machine-token — configured
// presence only, no secret material, no file paths.
type MachineTokenStatus struct {
	Daemon     DaemonInfo `json:"daemon"`
	Configured bool       `json:"configured"`
}

// MachineTokenRotateResponse is POST
// /v1/settings/machine-token/rotate. Token is the newly committed
// 64-hex credential. DurabilityConfirmed is false when the rename
// committed but the directory fsync failed — the credential IS active;
// only its durability is unconfirmed.
type MachineTokenRotateResponse struct {
	Daemon              DaemonInfo `json:"daemon"`
	Token               string     `json:"token"`
	DurabilityConfirmed bool       `json:"durabilityConfirmed"`
}
