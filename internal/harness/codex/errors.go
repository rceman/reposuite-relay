package codex

import (
	"github.com/rceman/reposuite-relay/internal/harness"
)

// The canonical adapter errors are defined once in internal/harness. The
// aliases below keep adapter-local call sites readable without importing
// the contract package at every use.
var (
	// ErrBusy means the session already has an in-flight turn.
	ErrBusy = harness.ErrBusy
	// ErrNoActiveTurn means cancel was requested with no in-flight turn.
	ErrNoActiveTurn = harness.ErrNoActiveTurn
	// ErrNativeSessionLost means exact resume of the recorded native
	// session failed. Relay never substitutes a different thread.
	ErrNativeSessionLost = harness.ErrNativeSessionLost
	// ErrNoSuchInput means the requested-input ID is unknown or already
	// resolved.
	ErrNoSuchInput = harness.ErrNoSuchInput
	// ErrRuntimeUnavailable means the harness runtime could not be started.
	ErrRuntimeUnavailable = harness.ErrRuntimeUnavailable
	// ErrUnsupported means the operation is not supported by this harness.
	ErrUnsupported = harness.ErrUnsupported
)
