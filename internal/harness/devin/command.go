package devin

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/harness/acp"
)

// DefaultCommand resolves the installed Devin CLI and its verified ACP
// subcommand, plus the verified process-level model flag:
//
//	devin acp [--model <MODEL>]
//
// `--model` is documented as the default model for every NEW ACP session, which
// is exactly why Relay partitions Devin runtimes by model instead of claiming a
// live per-session mutation.
func DefaultCommand(model string) (acp.Command, error) {
	path, err := exec.LookPath("devin")
	if err != nil {
		return acp.Command{}, fmt.Errorf("devin executable: %w", err)
	}
	args := []string{"acp"}
	if model != "" {
		if err := validateModelArg(model); err != nil {
			return acp.Command{}, err
		}
		args = append(args, "--model", model)
	}
	return acp.Command{Path: path, Args: args}, nil
}

// validateModelArg bounds a client-supplied model value before it becomes a
// process argument. The executable is always Relay's own; the model is only ever
// a VALUE, never a flag and never shell-interpreted.
func validateModelArg(model string) error {
	if len(model) > 128 {
		return fmt.Errorf("%w: model value too long", harness.ErrInvalidConfig)
	}
	if strings.HasPrefix(model, "-") {
		return fmt.Errorf("%w: model value must not look like a flag", harness.ErrInvalidConfig)
	}
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == ':', r == '/', r == ' ':
		default:
			return fmt.Errorf("%w: model value contains an unsupported character", harness.ErrInvalidConfig)
		}
	}
	return nil
}
