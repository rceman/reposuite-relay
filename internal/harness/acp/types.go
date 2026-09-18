package acp

import "encoding/json"

// ClientInfo identifies Relay to the agent (the ACP `Implementation`
// shape: name and version are required by both implementations).
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities is what Relay announces. Relay advertises no
// filesystem/terminal capability: it is not an editor host, and an agent
// that needs one must fail closed rather than be silently unsupported.
type ClientCapabilities struct {
	Terminal bool `json:"terminal"`
}

// InitializeParams is the handshake request.
type InitializeParams struct {
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *ClientInfo        `json:"clientInfo,omitempty"`
	ProtocolVersion    int                `json:"protocolVersion"`
}

// InitializeResult is the handshake response: the ACTUAL capabilities the
// runtime reports. Relay retains them and never assumes a capability the
// runtime did not advertise.
type InitializeResult struct {
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         *ClientInfo       `json:"agentInfo,omitempty"`
	AuthMethods       []json.RawMessage `json:"authMethods,omitempty"`
	ProtocolVersion   int               `json:"protocolVersion"`
}

// AgentCapabilities is the agent capability set.
type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

// PromptCapabilities is the prompt content the agent accepts.
type PromptCapabilities struct {
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Image           bool `json:"image"`
}

// MCPCapabilities is the MCP transport the agent accepts.
type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// SessionCapabilities is a presence-only capability set: each field is an
// (empty) object when the agent supports that operation and absent
// otherwise. Has reports presence.
type SessionCapabilities struct {
	AdditionalDirectories json.RawMessage `json:"additionalDirectories,omitempty"`
	Close                 json.RawMessage `json:"close,omitempty"`
	Fork                  json.RawMessage `json:"fork,omitempty"`
	List                  json.RawMessage `json:"list,omitempty"`
	Resume                json.RawMessage `json:"resume,omitempty"`
}

// Has reports whether a presence-only capability object was advertised.
func Has(v json.RawMessage) bool { return len(v) > 0 }

// SessionInfo is one native session record from `session/list`.
type SessionInfo struct {
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Cwd                   string   `json:"cwd,omitempty"`
	SessionID             string   `json:"sessionId"`
	Title                 string   `json:"title,omitempty"`
	UpdatedAt             string   `json:"updatedAt,omitempty"`
}

// ListSessionsParams is the `session/list` request (capability-gated; Relay
// never uses it to guess an identity — only exact IDs are ever resumed).
type ListSessionsParams struct {
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Cursor                string   `json:"cursor,omitempty"`
	Cwd                   string   `json:"cwd,omitempty"`
}

// ListSessionsResult is the `session/list` response.
type ListSessionsResult struct {
	NextCursor string        `json:"nextCursor,omitempty"`
	Sessions   []SessionInfo `json:"sessions"`
}
