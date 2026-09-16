package events

// Subscribe registers a live subscriber atomically with the replay
// prefix: under the session lock it captures replayable events with
// Seq > afterSeq and registers the queue, so no published event can slip
// between replay capture and subscription.
//
// Exactness is decided by the explicit replay floor, not by ring
// arithmetic: exact replay requires replayFloor <= afterSeq <= cursor.
// Below the floor the ring (or the previous generation) may have dropped
// events; above the cursor the request is from the future. Both are
// stable errors, never a silently incomplete stream.
func (s *Session) Subscribe(afterSeq uint64) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if afterSeq > s.cursor {
		return nil, ErrCursorAhead
	}
	if afterSeq < s.floor {
		return nil, ErrCursorTooOld
	}
	sub := &Subscription{ch: make(chan Event, SubscriberQueueSize)}
	for i := 0; i < s.count; i++ {
		ev := s.ring[(s.head+i)%len(s.ring)]
		if ev.Seq > afterSeq {
			sub.Replay = append(sub.Replay, ev)
		}
	}
	s.subs[sub] = struct{}{}
	return sub, nil
}

// Unsubscribe ends a subscription (client disconnect).
func (s *Session) Unsubscribe(sub *Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		sub.end(nil)
	}
}

// ID returns the RelaySession ID this event state belongs to.
func (s *Session) ID() string { return s.id }

// SubscriberCount returns the number of live subscribers (test seam).
func (s *Session) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// close ends all subscribers. Durable state and files are untouched; no
// file handles are held between operations.
func (s *Session) close(reason error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subs = map[*Subscription]struct{}{}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.end(reason)
	}
}
