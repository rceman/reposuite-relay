package acp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ServerConfig wires the agent→client half of one ACP runtime. It is the only
// place Relay's policy meets the protocol: everything else here is protocol
// mechanics.
type ServerConfig struct {
	// Client identifies Relay in the handshake.
	Client ClientInfo
	// Approve answers an approval request for one session. A nil policy
	// fails closed (outcome cancelled): an approval Relay cannot classify is
	// never granted.
	Approve func(sessionID string, params RequestPermissionParams) RequestPermissionResult
	// OnUpdate delivers one streaming update in arrival order. sessionID is
	// the NATIVE ACP session id the notification carries, so the owner must
	// resolve it to its own session identity. It is called on the transport
	// reader goroutine: an implementation must not block.
	OnUpdate func(sessionID string, update Update)
}

// unimplementedMethods are the agent→client requests Relay deliberately does
// not implement. Relay is not an editor or terminal host, so each gets an
// explicit protocol error instead of silence or a fabricated success.
var unimplementedMethods = []string{
	"fs/read_text_file",
	"fs/write_text_file",
	"terminal/create",
	"terminal/output",
	"terminal/wait_for_exit",
	"terminal/kill",
	"terminal/release",
	"elicitation/create",
	"elicitation/complete",
}

// Configure installs the agent→client handlers before Serve starts.
func (s *Server) Configure(cfg ServerConfig) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()

	conn := s.conn
	conn.SetNotificationHandler(s.handleNotification)
	conn.SetHandler(MethodRequestPermission, s.handlePermission)
	for _, method := range unimplementedMethods {
		method := method
		conn.SetHandler(method, func(_ context.Context, _ int64, _ json.RawMessage) (any, error) {
			return nil, fmt.Errorf("unsupported method %s: relay is not an editor or terminal host", method)
		})
	}
}

// handleNotification routes `session/update` to the owning session handle.
func (s *Server) handleNotification(method string, params json.RawMessage) {
	if method != MethodSessionUpdate {
		return
	}
	n, err := DecodeUpdate(params)
	if err != nil {
		return
	}
	if h, ok := s.Session(n.SessionID); ok {
		h.applyUpdate(n.Update)
	}
	s.cfgMu.Lock()
	onUpdate := s.cfg.OnUpdate
	s.cfgMu.Unlock()
	if onUpdate != nil {
		onUpdate(n.SessionID, n.Update)
	}
}

// handlePermission applies the approval policy. The decision is synchronous
// (no deferred response) so the agent is never left waiting on Relay.
func (s *Server) handlePermission(_ context.Context, _ int64, params json.RawMessage) (any, error) {
	var p RequestPermissionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return Cancelled(), nil
	}
	s.cfgMu.Lock()
	approve := s.cfg.Approve
	s.cfgMu.Unlock()
	if approve == nil {
		return Cancelled(), nil
	}
	return approve(p.SessionID, p), nil
}

// RegisterSession adds a session handle to the runtime's routing table.
func (s *Server) RegisterSession(h *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*Session{}
	}
	s.sessions[h.ID()] = h
}

// UnregisterSession removes a session handle.
func (s *Server) UnregisterSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// Session returns a registered session handle.
func (s *Server) Session(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.sessions[id]
	return h, ok
}
