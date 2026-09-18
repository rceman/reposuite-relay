package opencode

import (
	"fmt"
	"os/exec"

	"github.com/rceman/reposuite-relay/internal/harness/acp"
)

// DefaultCommand resolves the installed OpenCode CLI and its verified ACP
// subcommand. It is the only production command Relay ever runs for this
// harness: `opencode acp` on stdio. Relay never runs `opencode serve`, the
// TUI, or any other OpenCode surface.
func DefaultCommand() (acp.Command, error) {
	path, err := exec.LookPath("opencode")
	if err != nil {
		return acp.Command{}, fmt.Errorf("opencode executable: %w", err)
	}
	return acp.Command{Path: path, Args: []string{"acp"}}, nil
}
