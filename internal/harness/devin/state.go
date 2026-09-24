package devin

// Per-session adapter state (daemon-memory only; durability lives in
// session.json via materialize).

import (
	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/session"
)

// sessState is the adapter's per-session state; nothing here is durable.
type sessState struct {
	m *session.Managed
	// srv/handle is the runtime generation hosting the session; nil means
	// COLD.
	srv    *acp.Server
	handle *acp.Session
	// liveRuntimeKey is the runtime key of the generation that currently
	// owns handle. Ownership is NEVER reconstructed from the desired model:
	// a handle whose generation was partitioned for another process model
	// must not serve this session.
	liveRuntimeKey string
	// nativeID is the exact native slug in force. Until the first completed
	// turn it is PROVISIONAL: in memory only, never durable, because a
	// zero-turn Devin session cannot be resumed.
	nativeID string
	// materialized is true once a completed turn proved the slug resumable.
	materialized bool
	// turn is the in-flight turn (nil when idle), turnID is its Relay ID.
	turn   *acp.Turn
	turnID string
	// metrics is the last-known accounting.
	metrics *harness.SessionMetrics
	// tools tracks each live native tool call — updates do not repeat
	// kind/locations, so completion and source mapping need the values
	// remembered at tool_call time. Daemon-memory only.
	tools map[string]acp.ToolMeta
}
