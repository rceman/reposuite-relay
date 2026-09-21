package adminauth

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSessionLifecycleAndExpiry(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m, err := NewManager(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	token, s, err := m.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("token len %d, want 64 hex", len(token))
	}
	if len(s.CSRFToken) != 64 {
		t.Fatalf("csrf len %d, want 64 hex", len(s.CSRFToken))
	}
	got, ok := m.Lookup(token)
	if !ok || got.Username != "admin" {
		t.Fatal("fresh session not found")
	}
	// Raw token is never retained in the stored record beyond its digest.
	// Idle expiry.
	clock.Advance(SessionIdleTimeout + time.Second)
	if _, ok := m.Lookup(token); ok {
		t.Fatal("idle session survived")
	}
	// Absolute expiry beats activity: keep the session active with lookups
	// inside the idle window, but it still dies at the absolute bound.
	token2, _, _ := m.CreateSession("admin")
	created := clock.Now()
	for {
		clock.Advance(25 * time.Minute)
		if !clock.Now().Before(created.Add(SessionAbsoluteTimeout)) {
			break
		}
		if _, ok := m.Lookup(token2); !ok {
			t.Fatalf("session died early at %v", clock.Now().Sub(created))
		}
	}
	if _, ok := m.Lookup(token2); ok {
		t.Fatal("session survived absolute expiry")
	}
	// Garbage and unknown tokens rejected.
	if _, ok := m.Lookup("not-hex"); ok {
		t.Fatal("garbage token resolved")
	}
	if _, ok := m.Lookup("00"); ok {
		t.Fatal("short token resolved")
	}
}

func TestSessionRevoke(t *testing.T) {
	m, _ := NewManager(nil)
	token, _, _ := m.CreateSession("admin")
	m.Revoke(token)
	if _, ok := m.Lookup(token); ok {
		t.Fatal("revoked session still resolves")
	}
	// Double revoke is a deterministic no-op.
	m.Revoke(token)
	m.Revoke("zz")
}

func TestSessionStoreBounded(t *testing.T) {
	m, _ := NewManager(nil)
	tokens := make([]string, 0, MaxSessions+4)
	for i := 0; i < MaxSessions+4; i++ {
		tok, _, err := m.CreateSession("admin")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	if got := m.SessionCount(); got != MaxSessions {
		t.Fatalf("session count %d exceeds bound %d", got, MaxSessions)
	}
	// Oldest sessions were revoked.
	for _, tok := range tokens[:4] {
		if _, ok := m.Lookup(tok); ok {
			t.Fatal("oldest session was not evicted at capacity")
		}
	}
	if _, ok := m.Lookup(tokens[len(tokens)-1]); !ok {
		t.Fatal("newest session missing")
	}
}

func TestCSRFAndFormTokens(t *testing.T) {
	m, _ := NewManager(nil)
	_, s, _ := m.CreateSession("admin")
	if !m.CheckCSRF(s, s.CSRFToken) {
		t.Fatal("session CSRF rejected")
	}
	if m.CheckCSRF(s, "deadbeef") {
		t.Fatal("foreign CSRF accepted")
	}
	tok := m.FormToken("setup")
	if !m.CheckFormToken("setup", tok) {
		t.Fatal("setup form token rejected")
	}
	if m.CheckFormToken("login", tok) {
		t.Fatal("form token crossed actions")
	}
	if m.CheckFormToken("setup", "deadbeef") {
		t.Fatal("garbage form token accepted")
	}
	// A new daemon generation rotates the form secret.
	m2, _ := NewManager(nil)
	if m2.CheckFormToken("setup", tok) {
		t.Fatal("form token survived daemon restart")
	}
}

func TestLoginThrottle(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m, _ := NewManager(clock.Now)
	for i := 0; i < LoginMaxFailures-1; i++ {
		m.RecordLoginFailure()
		if th := m.CheckLoginThrottle(); !th.Allowed {
			t.Fatalf("throttled after %d failures", i+1)
		}
	}
	m.RecordLoginFailure()
	th := m.CheckLoginThrottle()
	if th.Allowed {
		t.Fatal("not throttled at threshold")
	}
	if th.RetryAfter <= 0 || th.RetryAfter > LoginThrottleFor {
		t.Fatalf("Retry-After %v out of bounds", th.RetryAfter)
	}
	clock.Advance(LoginThrottleFor + time.Second)
	if th := m.CheckLoginThrottle(); !th.Allowed {
		t.Fatal("throttle did not release")
	}
	// Window expiry forgets old failures.
	clock.Advance(LoginFailureWindow + time.Second)
	m.RecordLoginFailure()
	if th := m.CheckLoginThrottle(); !th.Allowed {
		t.Fatal("stale failures still counted")
	}
	// Success clears everything.
	m.RecordLoginFailure()
	m.RecordLoginFailure()
	m.RecordLoginSuccess()
	if th := m.CheckLoginThrottle(); !th.Allowed {
		t.Fatal("success did not reset throttle")
	}
}
