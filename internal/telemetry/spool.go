package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The fallback spool is an exceptional outage mechanism — NOT a second
// telemetry database. It stores already-canonicalized events as bounded
// JSONL chunks under a Relay-owned, owner-private directory:
//
//	<spoolDir>/chunk-<seq>.jsonl
//
// Chunks are immutable once published (temp+fsync+rename+dirsync), replayed
// in name order, and deleted only after a RepoDex durable ACK (or counted
// loss of permanently rejected events). One file per chunk, never per event.
type spool struct {
	dir   string
	limit int64
	mu    sync.Mutex
	next  int // next chunk sequence
	bytes int64
}

type chunk struct {
	name string
	ids  []string
	raws [][]byte
	size int64
}

func openSpool(dir string, limit int64) *spool {
	return &spool{
		dir:   dir,
		limit: limit,
	}
}

// load scans existing chunks (crash recovery). Called once at sender start.
func (s *spool) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	max := 0
	for _, e := range ents {
		// Only committed chunk files count toward the bound — a torn
		// .tmp residue is recovered by the next write's O_EXCL path and
		// must not silently eat the budget.
		if !strings.HasPrefix(e.Name(), "chunk-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		s.bytes += fi.Size()
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e.Name(), "chunk-"), ".jsonl")); err == nil && n > max {
			max = n
		}
	}
	s.next = max
	return nil
}

// write persists a batch of canonical events as one immutable chunk.
// Returns false (caller counts loss) when the bound is exhausted.
func (s *spool) write(qs []queued) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var size int64
	for _, q := range qs {
		size += int64(len(q.raw)) + 1
	}
	if s.bytes+size > s.limit {
		return false
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false
	}
	s.next++
	name := fmt.Sprintf("chunk-%06d.jsonl", s.next)
	tmp := filepath.Join(s.dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	for _, q := range qs {
		if _, err := f.Write(q.raw); err != nil {
			f.Close()
			os.Remove(tmp)
			return false
		}
		if _, err := f.WriteString("\n"); err != nil {
			f.Close()
			os.Remove(tmp)
			return false
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return false
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		os.Remove(tmp)
		return false
	}
	syncDir(s.dir)
	s.bytes += size
	return true
}

// peek returns the oldest chunk without consuming it.
func (s *spool) peek() (*chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.names()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	b, err := readBounded(filepath.Join(s.dir, names[0]))
	if err != nil {
		return nil, err
	}
	c := &chunk{
		name: names[0],
		size: int64(len(b)),
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		raw := []byte(line)
		c.raws = append(c.raws, raw)
		c.ids = append(c.ids, eventIDOf(raw))
	}
	return c, nil
}

// drop removes a chunk after durable ACK or accounted permanent loss.
func (s *spool) drop(c *chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(filepath.Join(s.dir, c.name))
	s.bytes -= c.size
	if s.bytes < 0 {
		s.bytes = 0
	}
}

// quarantine sidelines the oldest chunk file when it cannot be read at
// all — renamed out of the replay set (never silently deleted), removed
// from the byte budget, and visible as a ".corrupt" residue.
func (s *spool) quarantine() {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.names()
	if err != nil || len(names) == 0 {
		return
	}
	old := filepath.Join(s.dir, names[0])
	if fi, err := os.Stat(old); err == nil {
		s.bytes -= fi.Size()
		if s.bytes < 0 {
			s.bytes = 0
		}
	}
	_ = os.Rename(old, old+".corrupt")
}

func (s *spool) empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.names()
	return err != nil || len(names) == 0
}

func (s *spool) totalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func (s *spool) names() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "chunk-") && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// eventIDOf extracts event_id from an encoded canonical event.
func eventIDOf(raw []byte) string {
	var v struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.EventID
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
