package telemetry

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/rceman/reposuite-relay/internal/harness"
	"github.com/rceman/reposuite-relay/internal/session"
)

// seqBlockSize is the durable reservation quantum for telemetry sequences:
// one session.json write amortizes a block of allocations. Gaps after a
// crash are legal; reuse is never possible.
const seqBlockSize = 256

// Deps wires the telemetry service into the daemon. All callbacks are
// required for an enabled service; a disabled service ignores them.
type Deps struct {
	// SpoolDir is the Relay-owned outage fallback directory (created lazily,
	// owner-private).
	SpoolDir string
	// ReserveSeq durably raises a session's telemetry sequence watermark
	// under the session's MetaMu (daemon materialize path).
	ReserveSeq func(m *session.Managed, mark uint64) error
	// ProjectID resolves the current Relay presentation grouping for a
	// session cwd ("" when ungrouped). Presentation authority only — never
	// Task authority.
	ProjectID func(cwd string) string
	// AdapterVersion is Relay's version string (source.adapter_version).
	AdapterVersion string
	// Now is a clock seam for tests.
	Now func() time.Time
	// Sleep is a backoff seam for tests (must be fast/deterministic there).
	Sleep func(d time.Duration)
	// HTTPClient is injectable for tests; production uses a bounded client.
	HTTPClient Doer
}

// Doer is the transport seam — production is the loopback HTTP client.
type Doer interface {
	Do(req *Request) (*Reply, error)
}

// sessionState is the per-session telemetry identity: separate sequence
// domain, producer provenance, and the one-shot session_started marker.
type sessionState struct {
	next           uint64 // next unallocated telemetry seq
	hwm            uint64 // durably reserved watermark
	startedEmitted bool   // seq 0 emitted this daemon generation
	source         Source
}

// Service is the harness-neutral telemetry pipeline. It is always
// constructed; when disabled every Observe* method is a no-op, so agent
// execution never depends on RepoDex. All methods are nil-receiver safe so
// adapters need no plumbing branches.
type Service struct {
	deps Deps
	cfg  Config

	mu       sync.Mutex
	sessions map[string]*sessionState // by RelaySession ID
	queue    []queued                 // bounded healthy-path queue
	overflow []queued                 // spillover drained to spool by sender
	closed   bool
	wake     chan struct{}
	spool    *spool // sender-owned outage fallback

	hc health // counters

	done chan struct{} // closed when the sender goroutine exits
}

// queued is one canonicalized event retained verbatim for retry/spool.
type queued struct {
	id  string // event_id — identity for reject pruning/dedup
	raw []byte // canonical encoded event — never regenerated
}

// New builds the service. cfg zero values take the documented defaults.
func New(cfg Config, deps Deps) *Service {
	cfg = cfg.withDefaults()
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}
	return &Service{
		deps:     deps,
		cfg:      cfg,
		sessions: map[string]*sessionState{},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Enabled reports whether RepoDex telemetry is configured on.
func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

// sess returns (creating if needed) the per-session telemetry state. The
// allocator resumes strictly after the durable watermark — never reusing.
func (s *Service) sess(m *session.Managed) *sessionState {
	rs := m.Snapshot()
	if st := s.sessions[rs.ID]; st != nil {
		return st
	}
	st := &sessionState{
		next: rs.TelemetrySeqWatermark + 1,
		hwm:  rs.TelemetrySeqWatermark,
		source: Source{
			Runtime:        rs.Harness,
			Adapter:        "reposuite-relay",
			AdapterVersion: s.deps.AdapterVersion,
		},
	}
	s.sessions[rs.ID] = st
	return st
}

// allocLocked returns the next telemetry seq for a session, durably
// reserving a fresh block when exhausted. Caller holds s.mu.
func (s *Service) allocLocked(m *session.Managed, st *sessionState) (uint64, error) {
	if st.next > st.hwm {
		newMark := st.hwm + seqBlockSize
		if err := s.deps.ReserveSeq(m, newMark); err != nil {
			return 0, err
		}
		st.hwm = newMark
	}
	seq := st.next
	st.next++
	return seq, nil
}

// ForgetSession drops per-session telemetry state on session deletion.
// Already-queued events still deliver — only the allocator state is
// released, so a recreated session starts a fresh sequence domain.
func (s *Service) ForgetSession(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// emit canonicalizes one observation and enqueues it. The
// observe→canonicalize→enqueue path performs no network I/O. A nil or
// disabled service is a no-op; allocation/validation failures are counted
// and degrade health rather than breaking the agent.
func (s *Service) emit(m *session.Managed, typ string, data any) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	s.hc.observed.Add(1)
	raw, err := json.Marshal(data)
	if err != nil {
		s.hc.canonErr.Add(1)
		return
	}
	rawData := json.RawMessage(raw) // []byte would marshal as base64
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	st := s.sess(m)
	// seq 0 is reserved for session_started: emitted lazily on the
	// session's first telemetry observation. Content uses only immutable
	// facts, so a daemon restart re-emits an identical event and RepoDex
	// dedups it by event_id — deterministic and idempotent.
	if !st.startedEmitted {
		rs := m.Snapshot()
		if st.hwm == 0 {
			// First-ever allocation: reserve a block covering seq 0 and
			// consume it, so dynamic telemetry starts at seq 1.
			if err := s.deps.ReserveSeq(m, seqBlockSize); err != nil {
				s.mu.Unlock()
				s.hc.canonErr.Add(1)
				return
			}
			st.hwm = seqBlockSize
			st.next = 1
		}
		st.startedEmitted = true
		// hwm>0 means seq 0 was already allocated historically — re-emit
		// identical content; dedup makes it safe.
		s.enqueueLocked(s.build(m, st, SeqSessionStarted,
			TypeSessionStarted, SessionStartedData{Repository: rs.Cwd}))
	}
	seq, err := s.allocLocked(m, st)
	if err != nil {
		s.mu.Unlock()
		s.hc.canonErr.Add(1)
		return
	}
	s.enqueueLocked(s.build(m, st, seq, typ, rawData))
	s.mu.Unlock()
	s.notify()
}

// build constructs one canonical event. Caller holds s.mu.
func (s *Service) build(m *session.Managed, st *sessionState, seq uint64,
	typ string, data any) queued {
	rs := m.Snapshot()
	var rawData json.RawMessage
	switch v := data.(type) {
	case json.RawMessage:
		rawData = v
	default:
		rawData, _ = json.Marshal(v)
	}
	ev := Event{
		Schema:    SchemaID,
		EventID:   "", // set below
		SessionID: rs.ID,
		Sequence:  seq,
		Timestamp: s.deps.Now().Format(time.RFC3339Nano),
		Source:    st.source,
		Type:      typ,
		Data:      rawData,
	}
	// Provenance: only a PROVEN durable native identity is attached — never
	// a guessed one.
	if harness.ValidNativeID(rs.NativeSessionID) {
		ev.Source.NativeSessionID = rs.NativeSessionID
	}
	if seq != SeqSessionStarted && s.deps.ProjectID != nil {
		// Presentation grouping at emit time; absent for ungrouped and for
		// session_started (its content must stay immutable across restarts).
		if pid := s.deps.ProjectID(rs.Cwd); pid != "" {
			ev.ProjectID = pid
		}
	}
	ev.EventID = ev.SessionID + ":" + itoa(seq)
	if err := ev.Validate(); err != nil {
		s.hc.canonErr.Add(1)
		return queued{}
	}
	raw, err := ev.Encode()
	if err != nil {
		s.hc.canonErr.Add(1)
		return queued{}
	}
	s.hc.canonicalized.Add(1)
	return queued{
		id:  ev.EventID,
		raw: raw,
	}
}

// enqueueLocked appends to the bounded queue; overflow is drained to the
// fallback spool by the sender. Hard overflow counts an explicit loss —
// never a silent drop. Caller holds s.mu.
func (s *Service) enqueueLocked(q queued) {
	if q.id == "" {
		return
	}
	if len(s.queue) < s.cfg.QueueLimit {
		s.queue = append(s.queue, q)
		return
	}
	if len(s.overflow) < s.cfg.QueueLimit {
		s.overflow = append(s.overflow, q)
		return
	}
	s.hc.lost.Add(1)
	s.setStateLocked(StateDegraded)
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
