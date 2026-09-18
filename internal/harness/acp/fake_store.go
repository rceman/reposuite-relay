package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// fakeSessionRecord is one persisted native session in the fake store. The
// shape mirrors what matters for the adapter contract: an exact identity, the
// cwd it was created in, and whether it is resumable.
type fakeSessionRecord struct {
	ID           string   `json:"sessionId"`
	Cwd          string   `json:"cwd"`
	Turns        int      `json:"turns"`
	Messages     []string `json:"messages"`
	Model        string   `json:"model,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	Materialized bool     `json:"materialized"`
	CreatedAt    string   `json:"createdAt"`
}

// fakeStore is the fake agent's native session persistence. It is a real
// on-disk store so "exact resume" is genuinely cross-process: a second fake
// process (a new runtime generation) can only reattach to a session that the
// first one persisted.
type fakeStore struct {
	mu     sync.Mutex
	root   string
	vendor string
}

func newFakeStore(root, vendor string) (*fakeStore, error) {
	if root == "" {
		return nil, fmt.Errorf("%s is required", EnvFakeACPState)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &fakeStore{
		root:   root,
		vendor: vendor,
	}, nil
}

// requiresTurn reports whether this vendor needs a completed turn before a
// session is resumable (the verified Devin/OpenCode difference).
func (s *fakeStore) requiresTurn() bool { return s.vendor == FakeVendorDevin }

func (s *fakeStore) path(id string) string {
	return filepath.Join(s.root, id+".json")
}

// create allocates a new native session identity and persists it
// immediately (both vendors return a durable identity from session/new).
func (s *fakeStore) create(cwd string) (fakeSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 64; attempt++ {
		rec := fakeSessionRecord{
			ID:        s.newID(attempt),
			Cwd:       cwd,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if _, err := os.Stat(s.path(rec.ID)); err == nil {
			continue
		}
		if err := s.saveLocked(rec); err != nil {
			return rec, err
		}
		return rec, nil
	}
	return fakeSessionRecord{}, fmt.Errorf("could not allocate a fake session id")
}

// newID builds a deterministic-per-process native identity: `ses_*` for
// OpenCode (its verified shape) and a slug for Devin (its verified shape).
func (s *fakeStore) newID(attempt int) string {
	n := os.Getpid()*100 + attempt
	if s.vendor == FakeVendorDevin {
		return fmt.Sprintf("fake-session-%d", n)
	}
	return fmt.Sprintf("ses_%d", n)
}

func (s *fakeStore) load(id string) (fakeSessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *fakeStore) loadLocked(id string) (fakeSessionRecord, bool) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return fakeSessionRecord{}, false
	}
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		return fakeSessionRecord{}, false
	}
	var rec fakeSessionRecord
	if json.Unmarshal(raw, &rec) != nil {
		return fakeSessionRecord{}, false
	}
	return rec, true
}

func (s *fakeStore) save(rec fakeSessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(rec)
}

// saveLocked writes atomically: a partially written native session must never
// be observable as resumable.
func (s *fakeStore) saveLocked(rec fakeSessionRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := s.path(rec.ID) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(rec.ID))
}

// list returns every persisted session, sorted by identity.
func (s *fakeStore) list() []fakeSessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	out := []fakeSessionRecord{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, ok := s.loadLocked(strings.TrimSuffix(e.Name(), ".json"))
		if ok {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// appendTurn records a completed turn durably. The first completed turn is
// what makes a Devin session resumable.
func (s *fakeStore) appendTurn(id, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.loadLocked(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}
	rec.Turns++
	rec.Messages = append(rec.Messages, text)
	rec.Materialized = true
	return s.saveLocked(rec)
}

// fakeEvents records scenario observations in the state root so a test can
// assert on exactly what the agent saw (and that a resumed process did NOT
// receive a fresh session/new).
type fakeEvents struct {
	mu        sync.Mutex
	path      string
	seen      []string
	firstTurn chan struct{}
}

func newFakeEvents(root string) *fakeEvents {
	return &fakeEvents{
		path:      filepath.Join(root, "events.log"),
		firstTurn: make(chan struct{}),
	}
}

func (e *fakeEvents) record(kind, detail string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	line := kind + "\t" + detail + "\n"
	e.seen = append(e.seen, line)
	f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
	if kind == "turn-complete" {
		select {
		case <-e.firstTurn:
		default:
			close(e.firstTurn)
		}
	}
}

// ReadFakeEvents returns the recorded scenario observations for a state root.
func ReadFakeEvents(stateDir string) []string {
	raw, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// CountFakeEvents counts recorded observations of one kind.
func CountFakeEvents(stateDir, kind string) int {
	n := 0
	for _, line := range ReadFakeEvents(stateDir) {
		if strings.HasPrefix(line, kind+"\t") {
			n++
		}
	}
	return n
}

// FakeSessionIDs returns the native session identities persisted in a state
// root, sorted.
func FakeSessionIDs(stateDir string) []string {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}
