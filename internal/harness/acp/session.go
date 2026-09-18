package acp

import "encoding/json"

// ContentBlock is one ACP content block. Relay only sends and reads text
// blocks; other types are recognized but never fabricated.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextBlock builds a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{
		Type: "text",
		Text: text,
	}
}

// NewSessionParams is the `session/new` request. mcpServers is part of the
// verified shape (both implementations require the field), so Relay always
// sends an explicit empty array rather than omitting it.
type NewSessionParams struct {
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	Cwd                   string      `json:"cwd"`
	McpServers            []MCPServer `json:"mcpServers"`
}

// MCPServer is deliberately opaque: Relay configures no MCP servers, and a
// client can never inject one.
type MCPServer = json.RawMessage

// NewSessionResult is the `session/new` response: the exact native session
// identity plus the config/model/mode surface the runtime advertises.
type NewSessionResult struct {
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
	Models        *ModelState    `json:"models,omitempty"`
	Modes         *ModeState     `json:"modes,omitempty"`
	SessionID     string         `json:"sessionId"`
}

// LoadSessionParams is the `session/load` request: an EXACT native session
// identity. There is no "latest"/"newest"/list-and-guess variant anywhere in
// Relay.
type LoadSessionParams struct {
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	Cwd                   string      `json:"cwd"`
	McpServers            []MCPServer `json:"mcpServers"`
	SessionID             string      `json:"sessionId"`
}

// LoadSessionResult is the `session/load` response.
type LoadSessionResult struct {
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
	Models        *ModelState    `json:"models,omitempty"`
	Modes         *ModeState     `json:"modes,omitempty"`
	SessionID     string         `json:"sessionId"`
}

// PromptParams is the `session/prompt` request. Its response is the terminal
// turn result.
type PromptParams struct {
	MessageID string         `json:"messageId,omitempty"`
	Prompt    []ContentBlock `json:"prompt"`
	SessionID string         `json:"sessionId"`
}

// PromptResult is the `session/prompt` response: the terminal turn outcome.
type PromptResult struct {
	StopReason    string `json:"stopReason"`
	Usage         *Usage `json:"usage,omitempty"`
	UserMessageID string `json:"userMessageId,omitempty"`
}

// CancelParams is the `session/cancel` NOTIFICATION (no response).
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// Usage is the native token accounting. Every field is optional so an
// absent measurement is never reported as a real zero.
type Usage struct {
	CachedReadTokens  *int64 `json:"cachedReadTokens,omitempty"`
	CachedWriteTokens *int64 `json:"cachedWriteTokens,omitempty"`
	InputTokens       *int64 `json:"inputTokens,omitempty"`
	OutputTokens      *int64 `json:"outputTokens,omitempty"`
	ThoughtTokens     *int64 `json:"thoughtTokens,omitempty"`
	TotalTokens       *int64 `json:"totalTokens,omitempty"`
}

// ModelState is the native model surface: a current model plus the available
// models. Relay only ever selects a model the runtime advertised.
type ModelState struct {
	AvailableModels []ModelInfo `json:"availableModels"`
	CurrentModelID  string      `json:"currentModelId"`
}

// ModelInfo is one advertised model.
type ModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ModeState is the native mode surface.
type ModeState struct {
	AvailableModes []ModeInfo `json:"availableModes"`
	CurrentModeID  string     `json:"currentModeId"`
}

// ModeInfo is one advertised mode.
type ModeInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// HasMode reports whether the runtime advertises a mode with this exact ID.
func (m *ModeState) HasMode(id string) bool {
	if m == nil || id == "" {
		return false
	}
	for _, mode := range m.AvailableModes {
		if mode.ID == id {
			return true
		}
	}
	return false
}

// HasModel reports whether the runtime advertises this exact model ID.
func (m *ModelState) HasModel(id string) bool {
	if m == nil || id == "" {
		return false
	}
	for _, model := range m.AvailableModels {
		if model.ModelID == id {
			return true
		}
	}
	return false
}
