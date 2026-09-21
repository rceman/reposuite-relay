package adminauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Browser session policy. Sessions are memory-only by design: a daemon
// restart invalidates every browser session.
const (
	SessionIdleTimeout     = 30 * time.Minute
	SessionAbsoluteTimeout = 12 * time.Hour
	MaxSessions            = 16
	// Login throttle: MaxFailures within FailureWindow locks login for
	// ThrottleFor.
	LoginMaxFailures   = 5
	LoginFailureWindow = 5 * time.Minute
	LoginThrottleFor   = 30 * time.Second
)

// Session is one authenticated browser session. The raw token exists
// only in the browser cookie; the store indexes by SHA-256(token).
type Session struct {
	Username    string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time // absolute expiry
	CSRFToken   string
	tokenDigest [32]byte
}

// clock is the test seam for deterministic time.
type Clock func() time.Time

// Manager owns the mutable browser-auth state: the session table, the
// per-daemon form secret (setup/login CSRF), and login throttling. It is
// memory-only — nothing here is persisted.
type Manager struct {
	mu      sync.Mutex
	now     Clock
	secret  [32]byte // per-daemon form-token secret
	byToken map[[sha256.Size]byte]*Session
	// login throttle
	failures    []time.Time
	throttledTo time.Time
}

// NewManager builds the runtime auth state. now may be nil (real clock).
func NewManager(now Clock) (*Manager, error) {
	m := &Manager{
		now:     now,
		byToken: map[[sha256.Size]byte]*Session{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if _, err := rand.Read(m.secret[:]); err != nil {
		return nil, err
	}
	return m, nil
}

// FormToken renders the pre-auth CSRF token for one action ("setup" or
// "login") — stateless HMAC under the per-daemon secret, so the token
// rotates on daemon restart.
func (m *Manager) FormToken(action string) string {
	sum := hmac.New(sha256.New, m.secret[:])
	sum.Write([]byte("form:" + action))
	return hex.EncodeToString(sum.Sum(nil))
}

// CheckFormToken verifies a submitted form token in constant time.
func (m *Manager) CheckFormToken(action, token string) bool {
	return subtle.ConstantTimeCompare([]byte(m.FormToken(action)), []byte(token)) == 1
}

// CreateSession mints a browser session: random 256-bit token (returned
// for the cookie, never stored raw), a random CSRF token, bounded store.
// Policy: expired sessions are pruned first; if still at capacity the
// oldest session is revoked.
func (m *Manager) CreateSession(username string) (token string, _ *Session, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	var csrf [32]byte
	if _, err := rand.Read(csrf[:]); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(m.now())
	if len(m.byToken) >= MaxSessions {
		m.revokeOldestLocked()
	}
	digest := sha256.Sum256(raw[:])
	now := m.now()
	s := &Session{
		Username:    username,
		CreatedAt:   now,
		LastSeenAt:  now,
		ExpiresAt:   now.Add(SessionAbsoluteTimeout),
		CSRFToken:   hex.EncodeToString(csrf[:]),
		tokenDigest: digest,
	}
	m.byToken[digest] = s
	return hex.EncodeToString(raw[:]), s, nil
}

// Lookup resolves a raw cookie token to a live session, applying idle
// and absolute expiry and refreshing LastSeenAt. An expired session is
// removed and reports not found.
func (m *Manager) Lookup(token string) (*Session, bool) {
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	digest := sha256.Sum256(raw)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byToken[digest]
	if !ok {
		return nil, false
	}
	now := m.now()
	if now.After(s.ExpiresAt) || now.Sub(s.LastSeenAt) > SessionIdleTimeout {
		delete(m.byToken, digest)
		return nil, false
	}
	s.LastSeenAt = now
	return s, true
}

// CheckCSRF verifies a session-bound CSRF token in constant time.
func (m *Manager) CheckCSRF(s *Session, token string) bool {
	return subtle.ConstantTimeCompare([]byte(s.CSRFToken), []byte(token)) == 1
}

// Revoke drops the session that owns token, if it exists.
func (m *Manager) Revoke(token string) {
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return
	}
	m.mu.Lock()
	delete(m.byToken, sha256.Sum256(raw))
	m.mu.Unlock()
}

// pruneLocked drops expired sessions.
func (m *Manager) pruneLocked(now time.Time) {
	for k, s := range m.byToken {
		if now.After(s.ExpiresAt) || now.Sub(s.LastSeenAt) > SessionIdleTimeout {
			delete(m.byToken, k)
		}
	}
}

// revokeOldestLocked removes the session with the earliest CreatedAt.
func (m *Manager) revokeOldestLocked() {
	var oldest [sha256.Size]byte
	var at time.Time
	first := true
	for k, s := range m.byToken {
		if first || s.CreatedAt.Before(at) {
			oldest, at, first = k, s.CreatedAt, false
		}
	}
	if !first {
		delete(m.byToken, oldest)
	}
}

// LoginThrottleResult is the outcome of a throttling check.
type LoginThrottleResult struct {
	Allowed    bool
	RetryAfter time.Duration
}

// CheckLoginThrottle reports whether a login attempt may proceed.
func (m *Manager) CheckLoginThrottle() LoginThrottleResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Before(m.throttledTo) {
		return LoginThrottleResult{
			Allowed:    false,
			RetryAfter: m.throttledTo.Sub(now),
		}
	}
	return LoginThrottleResult{Allowed: true}
}

// RecordLoginFailure registers one failed attempt. When failures in the
// window reach the threshold the throttle engages.
func (m *Manager) RecordLoginFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cutoff := now.Add(-LoginFailureWindow)
	keep := m.failures[:0]
	for _, t := range m.failures {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	m.failures = append(keep, now)
	if len(m.failures) >= LoginMaxFailures {
		m.throttledTo = now.Add(LoginThrottleFor)
	}
}

// RecordLoginSuccess clears the failure state after a successful login.
func (m *Manager) RecordLoginSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = nil
	m.throttledTo = time.Time{}
}

// SessionCount reports the live session count (test seam).
func (m *Manager) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byToken)
}

var ErrSetupConflict = errors.New("admin credentials already configured")
