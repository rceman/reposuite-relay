package telemetry

import (
	"sync/atomic"
	"time"
)

// state is the telemetry connectivity condition reported to health.
type state int32

const (
	StateDisabled state = iota
	StateDisconnected
	StateConnected
	StateDegraded
	StateIncompatible
)

func (s state) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateDisconnected:
		return "disconnected"
	case StateConnected:
		return "connected"
	case StateDegraded:
		return "degraded"
	case StateIncompatible:
		return "incompatible"
	}
	return "unknown"
}

// health holds the observer-only telemetry counters. Everything here is
// process-memory only — never persisted, never a secret.
type health struct {
	observed      atomic.Int64
	canonicalized atomic.Int64
	acked         atomic.Int64
	duplicates    atomic.Int64
	rejected      atomic.Int64
	lost          atomic.Int64
	retries       atomic.Int64
	rediscoveries atomic.Int64
	canonErr      atomic.Int64
	spoolWrites   atomic.Int64
	spoolReplays  atomic.Int64
	batches       atomic.Int64
	state         atomic.Int32
	lastAckUnix   atomic.Int64
	instanceID    atomic.Value // string
	endpoint      atomic.Value // string
}

// Snapshot is the observer-only telemetry health projection served on
// GET /v1/telemetry/repodex. It never contains the service token or any
// credential.
type Snapshot struct {
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	InstanceID    string `json:"instanceId,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
	QueueDepth    int    `json:"queueDepth"`
	SpoolBytes    int64  `json:"spoolBytes"`
	Observed      int64  `json:"observed"`
	Canonicalized int64  `json:"canonicalized"`
	Acknowledged  int64  `json:"acknowledged"`
	Duplicates    int64  `json:"duplicates"`
	Rejected      int64  `json:"rejected"`
	Lost          int64  `json:"lost"`
	Retries       int64  `json:"retries"`
	Rediscoveries int64  `json:"rediscoveries"`
	LastAckAt     string `json:"lastAckAt,omitempty"`
}

func (h *health) load() (st state, inst, ep string, lastAck string) {
	st = state(h.state.Load())
	if v := h.instanceID.Load(); v != nil {
		inst, _ = v.(string)
	}
	if v := h.endpoint.Load(); v != nil {
		ep, _ = v.(string)
	}
	if u := h.lastAckUnix.Load(); u != 0 {
		lastAck = time.Unix(u, 0).UTC().Format(time.RFC3339)
	}
	return
}
