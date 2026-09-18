package opencode

import (
	"context"
	"errors"

	"github.com/rceman/reposuite-relay/internal/harness"
)

// bg is the test prompt context: turns are awaited through the canonical
// stream, not through a control-plane deadline.
func bg() context.Context { return context.Background() }

// text builds a Relay prompt command.
func text(s string) harness.PromptCommand { return harness.PromptCommand{Text: s} }

func isBusy(err error) bool { return errors.Is(err, harness.ErrBusy) }

func isNativeSessionLost(err error) bool { return errors.Is(err, harness.ErrNativeSessionLost) }

func isUnsupported(err error) bool { return errors.Is(err, harness.ErrUnsupported) }

func isInvalidConfig(err error) bool { return errors.Is(err, harness.ErrInvalidConfig) }

func isNoActiveTurn(err error) bool { return errors.Is(err, harness.ErrNoActiveTurn) }
