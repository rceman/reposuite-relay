// Package session holds the canonical session domain model and the
// daemon's in-memory registry.
//
// RelaySession is the durable logical identity (persisted by
// internal/store); HarnessRuntime is the ephemeral harness process/
// generation owned by the daemon. They are distinct (ADR-005): a session
// restored after a daemon restart has a RelaySession and NO runtime — the
// absent runtime IS the COLD state, not a fake process object.
//
// The domain knows nothing about the wire protocol: projection to
// protocol DTOs lives in internal/daemon.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// HarnessFixture is the deterministic same-binary child used to prove
// daemon/session ownership. It executes no client-supplied command and
// has no native durable identity.
const HarnessFixture = "fixture"

// HarnessCodex is the native Codex app-server harness (ADR-005): one
// shared app-server process, one exact native thread per Relay session.
const HarnessCodex = "codex"

// HarnessOpenCode is the OpenCode ACP harness: one shared `opencode acp`
// process speaking ACP JSON-RPC over stdio, one exact native `ses_*`
// session per Relay session.
const HarnessOpenCode = "opencode"

// HarnessDevin is the Devin ACP harness: `devin acp` over stdio, one
// runtime per process-level model, one exact native slug session per
// Relay session.
const HarnessDevin = "devin"

// Logical session states. Logical state is NOT process state: a session
// stays "idle" whether its runtime is warm or entirely absent.
const (
	// StateIdle means the session has no in-flight native work.
	StateIdle = "idle"
	// StateActive means a native turn is in flight.
	StateActive = "active"
	// StateWaitingInput means a native requested-input request is
	// unresolved — the mandatory runtime sleep blocker.
	StateWaitingInput = "waiting_input"
)

// HarnessRuntime states (ephemeral, never persisted).
const (
	RuntimeStarting = "starting"
	RuntimeWarm     = "warm"
	RuntimeActive   = "active"
	RuntimeStopping = "stopping"
	// RuntimeCold is the projected runtime state when a session has no
	// runtime at all — zero harness resources.
	RuntimeCold = "cold"
)

var (
	keyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	idRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidKey reports whether k is a safe session key: 1-64 chars of
// [A-Za-z0-9._-], first char alphanumeric.
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// ValidID reports whether id is a well-formed Relay session or runtime
// ID: 32 lowercase hex chars (128 bits).
func ValidID(id string) bool { return idRe.MatchString(id) }

// newID returns a collision-safe identity: 128 bits of crypto/rand,
// hex-encoded. Not derived from PID, clock, key, cwd, or native identity.
// Errors are returned, never panicked — a caller must be able to abort
// session creation cleanly when secure randomness is unavailable.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// NewSessionID returns a fresh durable RelaySession identity. Stable for
// the lifetime of the session and safe as a directory name.
func NewSessionID() (string, error) { return newID() }

// NewRuntimeID returns a fresh ephemeral HarnessRuntime identity. It is a
// separate semantic from the session ID — a runtime dies with its daemon
// and is never persisted.
func NewRuntimeID() (string, error) { return newID() }

// RelaySession is the canonical durable logical session. It survives
// client disconnects (CLI, Web Admin), runtime sleep/death, and relayd
// restart. Generation counts how many runtimes have ever been
// established for it (restart alone does not increment it).
type RelaySession struct {
	ID              string // stable Relay-owned session ID (crypto/rand)
	Key             string // human/client session key
	Harness         string // fixture today; codex/devin/opencode later
	Cwd             string // absolute client working directory
	NativeSessionID string // exact native resume identity; "" for fixture
	State           string // durable logical state (e.g. StateIdle)
	Model           string // optional last-known model; "" = unset
	Mode            string // optional last-known mode; "" = unset
	Generation      int    // runtimes ever established (0 = never awake)
	// SeqHighWatermark is durable reserved sequence space for canonical
	// session events: seq values up to and including it are permanently
	// consumed (allocated, transient, or gap). It is NOT "last event
	// emitted" — transient events consume seq without being persisted, so
	// a restart must resume strictly after this watermark to guarantee no
	// seq is ever reused.
	SeqHighWatermark uint64    `json:"seqHighWatermark,omitempty"`
	CreatedAt        time.Time // session creation
	UpdatedAt        time.Time // last durable metadata change
}

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

// Registry is the daemon-memory session authority: a mutex-guarded map
// keyed by session Key. Durable identity lives in the store; the registry
// holds the live view (sessions + optional runtimes).
//
// Besides the committed sessions it tracks two internal key holds that
// keep the public state model intact:
//
//	reserved — a creator holds the key between conflict check and spawn
//	stopping — a stop owner holds the key until deletion completes
//
// Both are invisible to clients: a reserved key is not yet a session and
// a stopping key still is one.
type Registry struct {
	mu       sync.Mutex
	byKey    map[string]*Managed
	reserved map[string]struct{}
	stopping map[string]struct{}
	closed   bool // set by Drain: no new Reserve/BeginStop ever succeeds
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byKey:    map[string]*Managed{},
		reserved: map[string]struct{}{},
		stopping: map[string]struct{}{},
	}
}

// NewRestored builds a registry from persisted RelaySessions loaded at
// daemon startup. Every restored session is valid and queryable with
// runtime=nil (COLD) — daemon restart never fabricates a runtime or a
// native identity. Duplicate keys or IDs, or malformed records, are a
// startup error: canonical durable sessions are never silently dropped.
func NewRestored(sessions []*RelaySession) (*Registry, error) {
	r := NewRegistry()
	ids := make(map[string]string, len(sessions))
	for _, s := range sessions {
		if s == nil {
			return nil, fmt.Errorf("restored session is nil")
		}
		if !ValidID(s.ID) {
			return nil, fmt.Errorf("restored session %q: invalid session id", s.Key)
		}
		if !ValidKey(s.Key) {
			return nil, fmt.Errorf("restored session %s: invalid key %q", s.ID, s.Key)
		}
		if prev, ok := ids[s.ID]; ok {
			return nil, fmt.Errorf("duplicate session id %s (keys %q and %q)", s.ID, prev, s.Key)
		}
		if _, ok := r.byKey[s.Key]; ok {
			return nil, fmt.Errorf("duplicate session key %q", s.Key)
		}
		ids[s.ID] = s.Key
		r.byKey[s.Key] = &Managed{Session: s}
	}
	return r, nil
}

// Reserve atomically claims key for creation BEFORE any process spawn.
// Exactly one concurrent caller wins; losers get false (→ SESSION_EXISTS)
// and must not spawn. Pair with Commit on success or Cancel on failure.
func (r *Registry) Reserve(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if _, ok := r.byKey[key]; ok {
		return false
	}
	if _, ok := r.reserved[key]; ok {
		return false
	}
	if _, ok := r.stopping[key]; ok {
		return false // stop in flight — key still owned
	}
	r.reserved[key] = struct{}{}
	return true
}

// Cancel releases a failed reservation so the key can be retried.
func (r *Registry) Cancel(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.reserved, key)
}

// Commit atomically turns the caller's reservation into the managed
// session. False if the caller does not hold the reservation.
func (r *Registry) Commit(key string, m *Managed) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.reserved[key]; !ok {
		return false
	}
	delete(r.reserved, key)
	r.byKey[key] = m
	return true
}

// BeginStop returns the managed session plus exclusive stop ownership, or
// false when the key is absent, reserved, or already being stopped (losers
// must not call Stop on the same runtime). The session remains queryable
// until CommitStop — it still exists until confirmed deleted.
func (r *Registry) BeginStop(key string) (*Managed, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	if _, ok := r.reserved[key]; ok {
		return nil, false
	}
	if _, ok := r.stopping[key]; ok {
		return nil, false
	}
	m, ok := r.byKey[key]
	if !ok {
		return nil, false
	}
	r.stopping[key] = struct{}{}
	return m, true
}

// CommitStop removes the session after its runtime is confirmed stopped
// and its durable record is deleted.
func (r *Registry) CommitStop(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stopping, key)
	delete(r.byKey, key)
}

// AbortStop releases stop ownership after a failed stop/delete: the
// session stays in the registry (possibly COLD after ClearRuntime) — the
// daemon retains authority instead of losing the managed entry.
func (r *Registry) AbortStop(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stopping, key)
}

// Get returns the managed session for key — including one mid-stop (it
// still exists until CommitStop confirms).
func (r *Registry) Get(key string) (*Managed, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byKey[key]
	return m, ok
}

// List returns all managed sessions sorted by key (deterministic).
func (r *Registry) List() []*Managed {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Managed, 0, len(r.byKey))
	for _, m := range r.byKey {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.Key < out[j].Session.Key })
	return out
}

// Len returns the number of managed sessions — durable COLD sessions
// count exactly like sessions with a live runtime.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Drain closes the registry and atomically removes every managed session
// (shutdown). After Drain, Reserve and BeginStop permanently fail — the
// daemon only calls it once all lifecycle handlers are quiescent, so no
// Commit can create a session and no other goroutine holds stop ownership
// while it runs. In-flight reservations/stop-holds are discarded: their
// owners have already exited or will find the key gone. Drained sessions
// keep their durable records — shutdown stops runtimes only.
func (r *Registry) Drain() []*Managed {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	out := make([]*Managed, 0, len(r.byKey))
	for _, m := range r.byKey {
		out = append(out, m)
	}
	r.byKey = map[string]*Managed{}
	r.reserved = map[string]struct{}{}
	r.stopping = map[string]struct{}{}
	return out
}
