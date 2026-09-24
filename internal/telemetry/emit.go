package telemetry

// Harness-neutral observation entry points. Each method takes plain
// params (observe.go), canonicalizes inside emit(), and never blocks on
// delivery — RepoDex I/O is the sender's concern.

import "github.com/rceman/reposuite-relay/internal/session"

// --- observation surface (harness-neutral) -----------------------------------

// ToolCallStarted records one observed native tool invocation.
func (s *Service) ToolCallStarted(m *session.Managed, p ToolStart) {
	if s == nil || !s.cfg.Enabled || p.CallID == "" {
		return
	}
	s.emit(m, TypeToolCallStarted, ToolCallStartedData{
		ToolCallID: p.CallID,
		ToolName:   p.ToolName,
		Category:   p.Category,
	})
}

// ToolCallCompleted records one observed tool completion.
func (s *Service) ToolCallCompleted(m *session.Managed, p ToolEnd) {
	if s == nil || !s.cfg.Enabled || p.CallID == "" {
		return
	}
	d := ToolCallCompletedData{
		ToolCallID: p.CallID,
		OK:         p.OK,
		Status:     p.Status,
		DurationMs: p.DurationMs,
	}
	switch {
	case len(p.Output) > 0:
		d.OutputBytes = int64v(len(p.Output))
		d.OutputDigest = Digest(p.Output)
	case p.OutputBytes != nil:
		d.OutputBytes = p.OutputBytes
	}
	s.emit(m, TypeToolCallDone, d)
}

// SourceObserved records one proven source-content delivery. It is emitted
// ONLY when actual content reached the agent — a bare path mention is
// never sufficient (FILENAME_ONLY_COUNTS_AS_SOURCE_OBSERVED = false).
func (s *Service) SourceObserved(m *session.Managed, p SourceObs) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	if p.Kind == "" || (len(p.Content) == 0 && p.Bytes == nil) {
		return // no provable delivered content — not an observation
	}
	d := SourceObservedData{
		Path:            p.Path,
		ObservationKind: p.Kind,
		ToolCallID:      p.CallID,
		LineStart:       p.LineFrom,
		LineEnd:         p.LineTo,
	}
	if len(p.Content) > 0 {
		d.Bytes = int64v(len(p.Content))
		d.ContentDigest = Digest(p.Content)
	} else {
		d.Bytes = p.Bytes
	}
	s.emit(m, TypeSourceObserved, d)
}

// ModelUsage records one authoritative usage measurement at an objective
// boundary. UsageEstimated is always true: a Relay turn is not a proven
// single model call, so the canonical proxy boundary is marked honestly;
// the token values themselves are copied exactly from provider accounting.
func (s *Service) ModelUsage(m *session.Managed, p Usage) {
	if s == nil || !s.cfg.Enabled || p.CallID == "" {
		return
	}
	s.emit(m, TypeModelCallDone, ModelCallCompletedData{
		ModelCallID:       p.CallID,
		Model:             p.Model,
		InputTokens:       p.Input,
		OutputTokens:      p.Output,
		CachedInputTokens: p.CachedIn,
		ReasoningTokens:   p.Reason,
		DurationMs:        p.Duration,
		Status:            p.Status,
		UsageEstimated:    true,
	})
}

// FinalAnswer records the runtime-visible completed agent output.
// AgentMessage emits agent_message for one completed native assistant
// message — verbatim content, digest/bytes measured from the delivered text.
func (s *Service) AgentMessage(m *session.Managed, content string) {
	s.emit(m, TypeAgentMessage, AgentMessageData{
		Role:          "assistant",
		Content:       content,
		ContentBytes:  int64v(len(content)),
		ContentDigest: Digest([]byte(content)),
	})
}

func (s *Service) FinalAnswer(m *session.Managed, content string) {
	if s == nil || !s.cfg.Enabled || content == "" {
		return
	}
	s.emit(m, TypeFinalAnswer, FinalAnswerData{
		Content:       content,
		ContentBytes:  int64v(len(content)),
		ContentDigest: Digest([]byte(content)),
	})
}

// SessionCompleted records a real terminal RelaySession lifecycle point —
// session deletion. Runtime sleep/death and turn completion are NOT
// session completion and never emit this.
func (s *Service) SessionCompleted(m *session.Managed, reason string) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	s.emit(m, TypeSessionCompleted, SessionCompletedData{Reason: reason})
}

// itoa formats a uint64 without importing strconv into the hot path twice.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
