package telemetry

import (
	"crypto/sha256"
	"path/filepath"
	"strings"
)

// Observation params — the harness-neutral inputs harness adapters hand to
// the telemetry service. Adapters extract these from their own native DTOs;
// telemetry never sees vendor wire types.

// ToolStart describes one objectively observed native tool invocation.
type ToolStart struct {
	CallID   string // native tool-call identity (Codex item id, ACP toolCallId)
	ToolName string // native tool name retained verbatim
	Category string // one of the Cat* constants — classified by the harness
}

// ToolEnd describes one objectively observed tool completion.
type ToolEnd struct {
	CallID      string
	ToolName    string // retained for events whose start was never observed
	Category    string
	OK          bool
	Status      string // native status passthrough (completed|failed|...)
	DurationMs  *int64
	Output      []byte // delivered output for bytes/digest — never emitted
	OutputBytes *int64 // explicit count when output bytes aren't held
}

// SourceObs describes one proven source-content delivery to the agent.
type SourceObs struct {
	Path     string // canonical repo-relative path; "" when unprovable
	Kind     string // one of the Obs* constants
	CallID   string // tool call that delivered the content
	Content  []byte // exact delivered fragment — hashed, never emitted raw
	Bytes    *int64 // explicit byte count when content isn't held
	LineFrom *int64
	LineTo   *int64
}

// Usage describes one authoritative usage measurement. Every token field is
// optional; absent native values stay absent — never reported as zero.
type Usage struct {
	// CallID is the objective boundary identity (a native turn id). It is
	// scoped "turn:<id>" by the caller when the boundary is a whole turn
	// rather than a proven single model call.
	CallID   string
	Model    string
	Status   string
	Duration *int64
	Input    *int64
	Output   *int64
	CachedIn *int64
	Reason   *int64
}

// Digest returns the sha256 hex of exact delivered content bytes. Relay
// hashes bytes the harness already delivered — it never opens files to
// enrich telemetry.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2], out[i*2+1] = hex[b>>4], hex[b&15]
	}
	return string(out)
}

func int64v(n int) *int64 {
	v := int64(n)
	return &v
}

// RelPath normalizes a native path to repository-relative when it sits
// under the session working root. Absolute paths outside the root and
// empty paths yield "" — RepoDex leaves path absent for external paths,
// and Relay never fabricates one.
func RelPath(p, cwd string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return ""
}
