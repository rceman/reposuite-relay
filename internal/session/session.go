// Package session holds the logical session model and the daemon's
// in-memory registry.
//
// Authority: the registry is daemon-memory only. A daemon restart loses all
// sessions — durable metadata and crash recovery are deliberately deferred
// to a later milestone (see docs/adr/ADR-002).
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/protocol"
)

// HarnessFixture is the only built-in harness profile in this milestone: a
// deterministic same-binary child used to prove daemon/session ownership.
// It executes no client-supplied command.
const HarnessFixture = "fixture"

// StateRunning is the only state a session can be in until hibernation and
// resume are implemented in a later milestone.
const StateRunning = "running"

var keyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidKey reports whether k is a safe session key: 1-64 chars of
// [A-Za-z0-9._-], first char alphanumeric.
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// NewRuntimeID returns a collision-safe session runtime identity
// (128 bits of crypto/rand, hex-encoded). Not derived from PID or time.
// Errors are returned, never panicked — a caller must be able to abort
// session creation cleanly when secure randomness is unavailable.
func NewRuntimeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Session is one lightweight logical record per managed session. It is
// created once when `serve` accepts a new key and is stable for the life of
// the logical session.
type Session struct {
	Key        string
	RuntimeID  string
	Harness    string
	Cwd        string
	State      string
	CreatedAt  time.Time
	Generation int
}

// ActiveGeneration is the ephemeral owned runtime for one awake generation.
// In this milestone a generation owns exactly one fixture child process;
// PTY/VirtualTerminal fields arrive with the harness milestone.
type ActiveGeneration struct {
	PID       int
	StartedAt time.Time
}

// Managed pairs a logical session with its active generation and the
// daemon-owned stop hook. Stop is set by the daemon and is never marshalled.
type Managed struct {
	Session    *Session
	Generation *ActiveGeneration
	Stop       func() error
}

// Info projects a Managed into the wire DTO.
func (m *Managed) Info() protocol.SessionInfo {
	s, g := m.Session, m.Generation
	return protocol.SessionInfo{
		Key:                 s.Key,
		RuntimeID:           s.RuntimeID,
		Harness:             s.Harness,
		Cwd:                 s.Cwd,
		State:               s.State,
		Generation:          s.Generation,
		PID:                 g.PID,
		CreatedAt:           s.CreatedAt.UTC().Format(time.RFC3339),
		GenerationStartedAt: g.StartedAt.UTC().Format(time.RFC3339),
	}
}

// Registry is the daemon-memory session authority: a mutex-guarded map.
// Deliberately not durable (no SQLite/JSON store/event log).
//
// Besides the committed sessions it tracks two internal key holds that keep
// the public state model (running/absent) intact:
//
//	reserved — a creator holds the key between conflict check and spawn
//	stopping — a stop owner holds the key until the child is reaped
//
// Both are invisible to clients: a reserved key is not yet a session and a
// stopping key still is one.
type Registry struct {
	mu       sync.Mutex
	byKey    map[string]*Managed
	reserved map[string]struct{}
	stopping map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byKey:    map[string]*Managed{},
		reserved: map[string]struct{}{},
		stopping: map[string]struct{}{},
	}
}

// Reserve atomically claims key for creation BEFORE any process spawn.
// Exactly one concurrent caller wins; losers get false (→ SESSION_EXISTS)
// and must not spawn. Pair with Commit on success or Cancel on failure.
func (r *Registry) Reserve(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Commit atomically turns the caller's reservation into the running managed
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
// must not call Stop on the same generation). The session remains queryable
// until CommitStop — it is still running until confirmed stopped.
func (r *Registry) BeginStop(key string) (*Managed, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// CommitStop removes the session after its generation is confirmed stopped.
func (r *Registry) CommitStop(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stopping, key)
	delete(r.byKey, key)
}

// AbortStop releases stop ownership after a failed Stop: the session stays
// in the registry — the daemon retains authority over the still-running
// generation instead of losing it.
func (r *Registry) AbortStop(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stopping, key)
}

// Get returns the managed session for key — including one mid-stop (it is
// still running until CommitStop confirms).
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

// Len returns the number of managed sessions.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Drain atomically removes and returns every managed session (shutdown).
func (r *Registry) Drain() []*Managed {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Managed, 0, len(r.byKey))
	for _, m := range r.byKey {
		out = append(out, m)
	}
	r.byKey = map[string]*Managed{}
	return out
}
