// Package paths resolves the RepoSuite Relay filesystem layout.
//
// Contract:
//
//	RepoSuite root   = $REPOSUITE_HOME (cleaned) or <user home>/.reposuite
//	Relay root       = <RepoSuite root>/relay
//	Run directory    = <Relay root>/run
//	Daemon socket    = <Run dir>/relayd.sock   (future; not created here)
//	Log directory    = <Relay root>/logs
//
// Relay state never lives under ~/.airelay and never reads AIRELAY_*.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths is the resolved RepoSuite/Relay filesystem layout.
type Paths struct {
	repoSuiteRoot string
}

// Resolve computes the layout from an explicit home directory and the
// REPOSUITE_HOME value ("" means unset). Pure: touches no filesystem.
func Resolve(home, envRepoSuiteHome string) Paths {
	root := envRepoSuiteHome
	if root == "" {
		root = filepath.Join(home, ".reposuite")
	}
	return Paths{repoSuiteRoot: filepath.Clean(root)}
}

// Current resolves the layout from the process environment.
func Current() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home: %w", err)
	}
	return Resolve(home, os.Getenv("REPOSUITE_HOME")), nil
}

// RepoSuiteRoot is the RepoSuite state root.
func (p Paths) RepoSuiteRoot() string { return p.repoSuiteRoot }

// RelayRoot is the Relay state root.
func (p Paths) RelayRoot() string { return filepath.Join(p.repoSuiteRoot, "relay") }

// RunDir holds runtime artifacts such as the daemon socket.
func (p Paths) RunDir() string { return filepath.Join(p.RelayRoot(), "run") }

// DaemonSocket is the future reposuite-relayd socket path.
func (p Paths) DaemonSocket() string { return filepath.Join(p.RunDir(), "relayd.sock") }

// LogDir holds Relay logs.
func (p Paths) LogDir() string { return filepath.Join(p.RelayRoot(), "logs") }

// Ensure creates the Relay state directories (relay, run, logs) with
// user-private permissions. It never modifies pre-existing entries' modes
// and never touches anything outside the resolved Relay root.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.RelayRoot(), p.RunDir(), p.LogDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
