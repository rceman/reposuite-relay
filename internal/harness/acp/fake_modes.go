package acp

// Fake-agent modes (continued from fake.go) — each is a deterministic
// personality selected via FakeConfig.Mode; no model call is ever made.

const (
	// FakeConfigSlow blocks the native config RPC until the test releases it
	// (by creating <state>/release-config), so the sleep-blocker behaviour of
	// a live config mutation is deterministic and sleep-free.
	FakeConfigSlow = "config-slow"
	// FakeFailTurn answers every prompt with a non-success stop reason
	// without the runtime dying: the turn fails but the generation lives.
	FakeFailTurn = "fail-turn"
	// FakeTools emits a tool_call lifecycle (read with content, a listing
	// without content, a failing execute) for telemetry-mapping tests.
	FakeTools = "tools"
)
