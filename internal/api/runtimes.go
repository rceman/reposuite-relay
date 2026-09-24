package api

import "github.com/rceman/reposuite-relay/internal/telemetry"

// GET /v1/runtimes — the global live harness-runtime inventory.
// Observer-only: reading it never wakes a COLD session, spawns a
// harness, attaches a native session, or mutates durable state.
// Activity fields reflect Relay-observed state only — they carry no
// claim about external child processes or background work.

// RuntimeBinding is one RelaySession bound to a runtime generation.
type RuntimeBinding struct {
	Key       string `json:"key"`
	SessionID string `json:"sessionId"`
	Activity  string `json:"activity"` // idle | active | waiting_input
	Mutating  bool   `json:"mutating"`
}

// RuntimeResources is a best-effort live process-tree measurement.
// Available=false means the sample could not be taken (process gone,
// read error, unsupported platform) — zero fields are not "zero usage".
type RuntimeResources struct {
	Available    bool  `json:"available"`
	PSSBytes     int64 `json:"pssBytes,omitempty"`
	RSSBytes     int64 `json:"rssBytes,omitempty"`
	ProcessCount int   `json:"processCount,omitempty"`
}

// RuntimeInfo is one live harness-runtime generation.
type RuntimeInfo struct {
	RuntimeID     string `json:"runtimeId"`
	Harness       string `json:"harness"`
	Shared        bool   `json:"shared"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	State         string `json:"state"` // warm | starting | stopping

	SessionCount       int `json:"sessionCount"`
	ActiveSessionCount int `json:"activeSessionCount"`
	WaitingInputCount  int `json:"waitingInputCount"`
	MutationCount      int `json:"mutationCount"`

	Sessions  []RuntimeBinding `json:"sessions"`
	Resources RuntimeResources `json:"resources"`
}

// RuntimeTotals aggregates the inventory. When MeasuredRuntimeCount <
// RuntimeCount the PSS/RSS totals cover only measured runtimes — clients
// must not present them as the full sum.
type RuntimeTotals struct {
	RuntimeCount         int   `json:"runtimeCount"`
	SessionCount         int   `json:"sessionCount"`
	ActiveSessionCount   int   `json:"activeSessionCount"`
	WaitingInputCount    int   `json:"waitingInputCount"`
	MeasuredRuntimeCount int   `json:"measuredRuntimeCount"`
	PSSBytes             int64 `json:"pssBytes"`
	RSSBytes             int64 `json:"rssBytes"`
}

// RuntimeList is the response of GET /v1/runtimes.
type RuntimeList struct {
	Daemon    DaemonInfo    `json:"daemon"`
	SampledAt string        `json:"sampledAt"`
	Runtimes  []RuntimeInfo `json:"runtimes"`
	Totals    RuntimeTotals `json:"totals"`
}

// TelemetryHealth is the response of GET /v1/telemetry/repodex — the
// observer-only RepoDex telemetry pipeline snapshot. It carries no
// credential material (no service.token, no bearer).
type TelemetryHealth struct {
	Daemon    DaemonInfo         `json:"daemon"`
	Telemetry telemetry.Snapshot `json:"telemetry"`
}
