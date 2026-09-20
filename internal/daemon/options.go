// Daemon options and startup bounds — split from daemon.go to keep the
// server lifecycle file cohesive and inside the token budget.
package daemon

import (
	"time"

	"github.com/rceman/reposuite-relay/internal/fixture"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/runtime"
	"github.com/rceman/reposuite-relay/internal/session"
	"github.com/rceman/reposuite-relay/internal/store"
)

// Bounds for the local control plane — phase-specific, never a global
// write deadline (NDJSON event streams are intentionally long-lived).
const (
	ReadHeaderTimeout = 5 * time.Second  // request header deadline
	IdleTimeout       = 60 * time.Second // keep-alive idle deadline
	WriteTimeout      = 10 * time.Second // per-response deadline (streams clear it)
	MaxRequestSize    = 64 << 10         // bounded JSON request bodies

	// QuiescenceTimeout bounds how long shutdown waits for already-running
	// lifecycle handlers (create/stop ops) to finish before the final
	// registry drain. Never unbounded, and never skipped — the drain must
	// not race lifecycle ownership.
	QuiescenceTimeout = 10 * time.Second
)

// Options configures a Daemon. Zero fields take production defaults; the
// seams exist so tests can count spawns, inject failures, and shorten
// deadlines deterministically without changing production behavior.
type Options struct {
	SelfExe      string // path used to spawn __fixture children
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	SpawnFixture func(selfExe, cwd string) (runtime.Harness, error)
	// Separate identity seams: the durable RelaySession ID and the
	// ephemeral HarnessRuntime ID are different semantics and never share
	// one value.
	RandSessionID func() (string, error)
	RandRuntimeID func() (string, error)
	// WrapStop decorates each session's runtime-stop function — the seam
	// tests use to pause an in-flight stop under shutdown.
	WrapStop func(stop func() error) func() error
	// CodexCommand resolves the app-server command. Production resolves
	// the installed Codex CLI; tests point at the deterministic fake
	// app-server (the `__fake-codex` mode).
	CodexCommand func() (codex.Command, error)
	// OpenCodeCommand resolves the `opencode acp` command. Production
	// resolves the installed CLI; tests point at the deterministic fake ACP
	// agent (the `__fake-acp` mode).
	OpenCodeCommand func() (acp.Command, error)
	// DevinCommand resolves the `devin acp` command for a process-level
	// model. Production resolves the installed CLI; tests point at the
	// deterministic fake ACP agent.
	DevinCommand func(model string) (acp.Command, error)
	// StoreHooks overrides store filesystem primitives — test seam only
	// for deterministic post-commit failure injection.
	StoreHooks *store.Hooks
}

func (o Options) withDefaults() Options {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = ReadHeaderTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = WriteTimeout
	}
	if o.SpawnFixture == nil {
		o.SpawnFixture = func(selfExe, cwd string) (runtime.Harness, error) {
			return fixture.Spawn(selfExe, cwd)
		}
	}
	if o.RandSessionID == nil {
		o.RandSessionID = session.NewSessionID
	}
	if o.RandRuntimeID == nil {
		o.RandRuntimeID = session.NewRuntimeID
	}
	return o.withAdapterDefaults()
}
