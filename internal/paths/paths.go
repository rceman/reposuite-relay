// Package paths resolves the RepoSuite Relay filesystem layout.
//
// Contract:
//
//	RepoSuite root   = $REPOSUITE_HOME (cleaned) or <user home>/.reposuite
//	Relay root       = <RepoSuite root>/relay
//	Config directory = <Relay root>/config     (durable configuration)
//	Relay config     = <Config dir>/relay.json (0600, stable endpoint)
//	Machine API token= <Config dir>/api.token  (0600, persistent bearer)
//	Run directory    = <Relay root>/run        (current-process state)
//	Daemon descriptor= <Run dir>/daemon.json  (0600, local authority)
//	Daemon lock      = <Run dir>/relayd.lock
//	Log directory    = <Relay root>/logs
//	Sessions store   = <Relay root>/sessions/<session-id>/
//
// Safety invariant: Relay state never lives under ~/.airelay and never
// reads AIRELAY_*. A configured RepoSuite root equal to, or contained in,
// <home>/.airelay is rejected.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths is the resolved RepoSuite/Relay filesystem layout.
type Paths struct {
	repoSuiteRoot string
}

// legacyAirelayRoot is the state directory of legacy Airelay; Relay must
// never store state inside it.
func legacyAirelayRoot(home string) string { return filepath.Join(home, ".airelay") }

// Resolve computes the layout from an explicit home directory and the
// REPOSUITE_HOME value ("" means unset). Pure: touches no filesystem.
// Returns an error when the configured root is relative (daemon identity
// must not depend on the invoking client's working directory) or overlaps
// the legacy Airelay state directory.
func Resolve(home, envRepoSuiteHome string) (Paths, error) {
	root := envRepoSuiteHome
	if root == "" {
		root = filepath.Join(home, ".reposuite")
	}
	if !filepath.IsAbs(root) {
		return Paths{}, fmt.Errorf(
			"REPOSUITE_HOME %q is not absolute; daemon identity requires an absolute path", root)
	}
	root = filepath.Clean(root)
	if err := checkNotLegacy(home, root); err != nil {
		return Paths{}, err
	}
	return Paths{repoSuiteRoot: root}, nil
}

// checkNotLegacy rejects root when it equals or descends from
// <home>/.airelay. Containment is path-component-aware (filepath.Rel):
// ".airelay2" and similar sibling names are NOT rejected.
func checkNotLegacy(home, root string) error {
	legacy := legacyAirelayRoot(filepath.Clean(home))
	rel, err := filepath.Rel(legacy, root)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", root, err)
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return fmt.Errorf(
			"REPOSUITE_HOME %q overlaps legacy Airelay state %q; Relay state must not live there",
			root, legacy)
	}
	return nil
}

// Current resolves the layout from the process environment.
func Current() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home: %w", err)
	}
	return Resolve(home, os.Getenv("REPOSUITE_HOME"))
}

// RepoSuiteRoot is the RepoSuite state root.
func (p Paths) RepoSuiteRoot() string { return p.repoSuiteRoot }

// RelayRoot is the Relay state root.
func (p Paths) RelayRoot() string { return filepath.Join(p.repoSuiteRoot, "relay") }

// ConfigDir holds durable Relay configuration (never process state).
func (p Paths) ConfigDir() string { return filepath.Join(p.RelayRoot(), "config") }

// RelayConfig is the persistent control-plane configuration: the stable
// loopback endpoint selected once at first successful start and reused
// on every later start.
func (p Paths) RelayConfig() string { return filepath.Join(p.ConfigDir(), "relay.json") }

// MachineToken is the persistent machine API credential: one opaque
// bearer for trusted machine clients (GPT Tunnel, automation). It is
// never published in the runtime descriptor.
func (p Paths) MachineToken() string { return filepath.Join(p.ConfigDir(), "api.token") }

// AdminCredentials is the persistent Web Admin credential: the admin
// username plus an Argon2id password hash. It contains no plaintext
// password and no browser session material.
func (p Paths) AdminCredentials() string { return filepath.Join(p.ConfigDir(), "admin.json") }

// PresentationConfig is the persistent Relay-local presentation domain:
// human-facing display/grouping metadata (currently the Project catalog).
// It is durable configuration — never process state, so it lives under
// config/, not run/ — and it never carries session lifecycle authority.
func (p Paths) PresentationConfig() string {
	return filepath.Join(p.ConfigDir(), "presentation.json")
}

// RunDir holds runtime artifacts such as the daemon socket.
func (p Paths) RunDir() string { return filepath.Join(p.RelayRoot(), "run") }

// DaemonDescriptor is the runtime descriptor path (ADR-006): a
// user-private file publishing the loopback endpoint, instance identity,
// and local bearer token of the current daemon generation.
func (p Paths) DaemonDescriptor() string { return filepath.Join(p.RunDir(), "daemon.json") }

// DaemonLock is the singleton ownership lock file (current Linux
// implementation detail; the invariant is one daemon per state root).
func (p Paths) DaemonLock() string { return filepath.Join(p.RunDir(), "relayd.lock") }

// LogDir holds Relay logs.
func (p Paths) LogDir() string { return filepath.Join(p.RelayRoot(), "logs") }

// DaemonLog is the detached daemon's stdio destination — written only
// when the process is spawned detached (start/auto-start); `serve` keeps
// the caller's stdio.
func (p Paths) DaemonLog() string { return filepath.Join(p.LogDir(), "relayd.log") }

// TelemetrySpool is the outage-fallback spool for canonical RepoDex
// telemetry events — created lazily, owner-private, durable across restart.
func (p Paths) TelemetrySpool() string {
	return filepath.Join(p.RelayRoot(), "telemetry-spool")
}

// SessionsDir is the durable session store root: one canonical directory
// per RelaySession ID. Per-session file layout is owned by internal/store.
func (p Paths) SessionsDir() string { return filepath.Join(p.RelayRoot(), "sessions") }

// Ensure creates the Relay state directories (relay, config, run, logs)
// with user-private permissions. It never modifies pre-existing entries'
// modes and never touches anything outside the resolved Relay root.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.RelayRoot(), p.ConfigDir(), p.RunDir(), p.LogDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
