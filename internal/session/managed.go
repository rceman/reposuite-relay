package session

import "sync"

// Managed is a durable RelaySession plus its daemon-side synchronization.
// It deliberately owns NO process state: harness processes belong to the
// RuntimeSupervisor (internal/runtime), which may host several sessions on
// one shared runtime. A managed session with no supervisor binding is
// COLD — zero harness resources.
type Managed struct {
	Session *RelaySession

	// MetaMu serializes durable metadata mutation (session.json replace)
	// for this session: seq-block reservation, model/mode, nativeSessionId,
	// generation, state updates. Lock order: MetaMu may be taken under the
	// events broker's per-session lock and under Registry.mu, and must
	// never be held while taking either of them.
	MetaMu sync.Mutex
}

// Snapshot returns a copy of the durable session metadata. Every read of a
// field the daemon may mutate concurrently (state, generation, model,
// mode, nativeSessionId, seq watermark) must go through Snapshot or hold
// MetaMu: durable fields are mutated by the harness adapter from its own
// goroutines, not only from HTTP handlers.
func (m *Managed) Snapshot() RelaySession {
	m.MetaMu.Lock()
	defer m.MetaMu.Unlock()
	return *m.Session
}
