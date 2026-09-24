package telemetry

import (
	"errors"
	"time"
)

// The sender is a single goroutine owning delivery: batch construction,
// RepoDex HTTP, ACK handling, retry/backoff, rediscovery, and spool I/O.
// Keeping all of it off the observation path is the core invariant — agent
// work never waits for RepoDex.

const (
	// backoff bounds rediscovery/transport retry cadence.
	backoffMin = 250 * time.Millisecond
	backoffMax = 30 * time.Second
	// shutdownDrain bounds graceful shutdown delivery attempts.
	shutdownDrain = 3 * time.Second
)

// Start launches the sender goroutine. Safe when disabled.
func (s *Service) Start() {
	if s == nil || !s.cfg.Enabled {
		return
	}
	go s.run()
}

// Shutdown performs a bounded drain: pending events get delivery attempts
// up to the drain bound, then everything unacknowledged moves to the
// fallback spool for replay after restart. Never hangs on RepoDex.
func (s *Service) Shutdown() {
	if s == nil || !s.cfg.Enabled {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.notify()
	<-s.done
}

// Health returns the observer-only health snapshot — never a credential.
func (s *Service) Health() Snapshot {
	if s == nil {
		return Snapshot{State: StateDisabled.String()}
	}
	snap := Snapshot{Enabled: s.cfg.Enabled}
	st, inst, ep, last := s.hc.load()
	snap.State = st.String()
	if !s.cfg.Enabled {
		snap.State = StateDisabled.String()
	}
	snap.InstanceID, snap.Endpoint, snap.LastAckAt = inst, ep, last
	s.mu.Lock()
	snap.QueueDepth = len(s.queue) + len(s.overflow)
	s.mu.Unlock()
	snap.Observed = s.hc.observed.Load()
	snap.Canonicalized = s.hc.canonicalized.Load()
	snap.Acknowledged = s.hc.acked.Load()
	snap.Duplicates = s.hc.duplicates.Load()
	snap.Rejected = s.hc.rejected.Load()
	snap.Lost = s.hc.lost.Load()
	snap.Retries = s.hc.retries.Load()
	snap.Rediscoveries = s.hc.rediscoveries.Load()
	if s.spool != nil {
		snap.SpoolBytes = s.spool.totalBytes()
	}
	return snap
}

func (s *Service) setStateLocked(st state) {
	s.hc.state.Store(int32(st))
}

func (s *Service) run() {
	defer close(s.done)
	// s.spool was assigned in New() (Health must never race a nil
	// pointer); only the FS scan happens here on the sender goroutine.
	_ = s.spool.load()
	s.setStateLocked(StateDisconnected)

	var ep *endpoint
	backoff := backoffMin
	for {
		s.mu.Lock()
		closed := s.closed
		idle := len(s.queue) == 0 && len(s.overflow) == 0 && s.spool.empty()
		s.mu.Unlock()
		if closed {
			break
		}
		if ep == nil {
			if e := s.connect(); e != nil {
				ep = e
				backoff = backoffMin
				continue
			}
			// Unreachable: push queue overflow to the spool so memory
			// stays bounded, then wait for work or a backoff tick.
			s.drainToSpool()
			s.wait(backoff)
			backoff = minDur(backoff*2, backoffMax)
			continue
		}
		if idle {
			s.wait(s.cfg.batchDelay())
			continue
		}
		if !s.deliverOnce(ep) {
			ep = nil
			s.hc.retries.Add(1)
			s.wait(backoff)
			backoff = minDur(backoff*2, backoffMax)
			continue
		}
		backoff = backoffMin
	}
	s.drainShutdown()
}

// wait sleeps until new work arrives or d elapses.
func (s *Service) wait(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.wake:
	case <-s.done:
	case <-t.C:
	}
}

// connect discovers the RepoDex endpoint and validates /v1/status.
// Returns nil while the service is absent/down/incompatible — telemetry
// reports the condition, agents are unaffected. A schema/contract
// mismatch reports "incompatible"; absence/transport failure reports
// "disconnected".
func (s *Service) connect() *endpoint {
	ep, err := discover(s.cfg.RepoDexStateDir())
	if err != nil {
		s.mu.Lock()
		if errors.Is(err, errContract) {
			s.setStateLocked(StateIncompatible)
		} else {
			s.setStateLocked(StateDisconnected)
		}
		s.mu.Unlock()
		return nil
	}
	if err := statusOK(ep, s.doer()); err != nil {
		s.mu.Lock()
		if errors.Is(err, errContract) {
			s.setStateLocked(StateIncompatible)
		} else {
			s.setStateLocked(StateDisconnected)
		}
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.hc.instanceID.Store(ep.desc.InstanceID)
	s.hc.endpoint.Store(ep.desc.Host + ":" + itoa(uint64(ep.desc.Port)))
	if s.hc.state.Load() != int32(StateDegraded) {
		s.setStateLocked(StateConnected)
	}
	s.hc.rediscoveries.Add(1)
	s.mu.Unlock()
	return ep
}

func (s *Service) doer() Doer {
	if s.deps.HTTPClient != nil {
		return s.deps.HTTPClient
	}
	return HTTPDoer{}
}

// deliverOnce sends one unit of work: the oldest spool chunk first
// (backlog is never overtaken by newer telemetry), then one queue batch.
// Returns false when RepoDex is unreachable — the caller drops the
// endpoint and rediscovers.
func (s *Service) deliverOnce(ep *endpoint) bool {
	c, err := s.spool.peek()
	if err != nil {
		// Unreadable spool chunk (torn/corrupt): never wedged behind it —
		// quarantine the file and count a visible loss rather than retry
		// the same unreadable file forever.
		s.spool.quarantine()
		s.hc.lost.Add(1)
		s.mu.Lock()
		s.setStateLocked(StateDegraded)
		s.mu.Unlock()
		return true
	}
	if c != nil {
		return s.sendBatchOnce(ep, c.raws, func(ok bool) {
			if ok {
				s.spool.drop(c)
				s.hc.spoolReplays.Add(1)
			}
		})
	}
	batch := s.takeBatch()
	if len(batch) == 0 {
		// Overflow exists only when the queue overflowed — spool it so
		// pending telemetry stays bounded even while connected-but-slow.
		s.drainToSpool()
		return true
	}
	raws := split(batch)
	return s.sendBatchOnce(ep, raws, func(ok bool) {
		if !ok {
			s.spoolBatch(batch)
		}
	})
}

// sendBatchOnce posts one batch and reconciles the ingest ACK. RepoDex
// gives every event in a 200 batch exactly one verdict — accepted and
// duplicates are durable, rejected are permanently invalid and counted
// lost; the batch is fully accounted and dropped either way. A transport
// failure leaves the batch to done(false) for spool/retry. A permanent
// rejection (non-auth 4xx) or contract violation drops the batch as lost
// — retrying verbatim can never succeed, and an unaccounted sum is a
// contract bug that must not wedge the pipeline.
func (s *Service) sendBatchOnce(ep *endpoint, raws [][]byte, done func(ok bool)) bool {
	rep, err := sendBatch(ep, s.doer(), raws)
	if err != nil {
		s.mu.Lock()
		switch {
		case errors.Is(err, errPermanent) || errors.Is(err, errContract):
			s.setStateLocked(StateDegraded)
		default:
			s.setStateLocked(StateDisconnected)
		}
		s.mu.Unlock()
		if errors.Is(err, errPermanent) || errors.Is(err, errContract) {
			s.hc.lost.Add(int64(len(raws)))
			done(true) // drop — retrying a definitive reject wedges the pipe
			return true
		}
		done(false)
		return false
	}
	s.hc.acked.Add(rep.Accepted)
	s.hc.duplicates.Add(rep.Duplicates)
	s.hc.rejected.Add(rep.Rejected)
	if unaccounted := int64(len(raws)) - rep.Accepted - rep.Duplicates - rep.Rejected; unaccounted != 0 {
		// RepoDex guarantees one verdict per event; a mismatch is a
		// contract violation — count the unaccounted tail lost and mark
		// the pipeline degraded rather than retrying committed events.
		if unaccounted > 0 {
			s.hc.lost.Add(unaccounted)
		}
		s.mu.Lock()
		s.setStateLocked(StateDegraded)
		s.mu.Unlock()
	}
	if rep.Rejected > 0 {
		s.hc.lost.Add(rep.Rejected)
		s.mu.Lock()
		s.setStateLocked(StateDegraded)
		s.mu.Unlock()
	}
	s.hc.batches.Add(1)
	s.hc.lastAckUnix.Store(s.deps.Now().Unix())
	done(true)
	return true
}

// takeBatch pulls up to the configured event/byte bounds off the queue.
func (s *Service) takeBatch() []queued {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []queued
	var bytes int64
	for len(s.queue) > 0 && len(out) < s.cfg.BatchMaxEvents {
		q := s.queue[0]
		if len(out) > 0 && bytes+int64(len(q.raw)) > int64(s.cfg.BatchMaxBytes) {
			break
		}
		out = append(out, q)
		bytes += int64(len(q.raw))
		s.queue = s.queue[1:]
	}
	return out
}

// drainToSpool moves queue+overflow into the fallback spool so an outage
// cannot grow memory without bound.
func (s *Service) drainToSpool() {
	s.mu.Lock()
	// Queue holds the older events — it fills first; overflow receives
	// newer spillover. Preserve emission order in the spool.
	rest := append(append([]queued{}, s.queue...), s.overflow...)
	s.queue, s.overflow = nil, nil
	s.mu.Unlock()
	if len(rest) > 0 {
		s.spoolBatch(rest)
	}
}

// spoolBatch persists an undelivered batch; failure counts explicit loss.
func (s *Service) spoolBatch(qs []queued) {
	if s.spool.write(qs) {
		s.hc.spoolWrites.Add(1)
		return
	}
	s.hc.lost.Add(int64(len(qs)))
	s.mu.Lock()
	s.setStateLocked(StateDegraded)
	s.mu.Unlock()
}

// drainShutdown attempts a bounded final delivery, then spools the rest.
func (s *Service) drainShutdown() {
	deadline := time.Now().Add(shutdownDrain)
	for time.Now().Before(deadline) {
		ep := s.connect()
		if ep == nil {
			break
		}
		s.mu.Lock()
		idle := len(s.queue) == 0 && len(s.overflow) == 0 && s.spool.empty()
		s.mu.Unlock()
		if idle {
			return
		}
		if !s.deliverOnce(ep) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	s.drainToSpool()
}

// split flattens a queued batch into raw events for the wire.
func split(qs []queued) [][]byte {
	raws := make([][]byte, 0, len(qs))
	for _, q := range qs {
		raws = append(raws, q.raw)
	}
	return raws
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
