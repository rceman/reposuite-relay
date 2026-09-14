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
func NewRuntimeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
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
type Registry struct {
	mu    sync.Mutex
	byKey map[string]*Managed
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byKey: map[string]*Managed{}} }

// Create inserts m; false if the key already exists.
func (r *Registry) Create(m *Managed) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byKey[m.Session.Key]; dup {
		return false
	}
	r.byKey[m.Session.Key] = m
	return true
}

// Get returns the managed session for key.
func (r *Registry) Get(key string) (*Managed, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byKey[key]
	return m, ok
}

// Remove deletes key and returns it if present.
func (r *Registry) Remove(key string) (*Managed, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byKey[key]
	if ok {
		delete(r.byKey, key)
	}
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
