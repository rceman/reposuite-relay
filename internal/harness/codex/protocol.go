// Package codex implements the Codex app-server harness adapter: a
// line-delimited JSON-RPC client over the app-server's stdio transport,
// plus the Relay-side mapping from native thread/turn notifications to
// canonical Relay events.
//
// Verified against the installed Codex CLI (codex-cli 0.154.0) and its
// generated JSON schema (`codex app-server generate-json-schema`). Every
// method and field used here exists in that schema; nothing is invented.
// The native store stays authoritative for conversation state — Relay
// persists only its own compact transcript and the exact native thread ID
// needed for resume.
package codex

import "encoding/json"

// MaxFrameBytes bounds one JSON-RPC frame in either direction. A larger
// frame fails the connection closed instead of allocating unboundedly.
const MaxFrameBytes = 4 << 20

// Server→client request methods (verified subset).
const (
	// MethodRequestUserInput is the requested-input server request.
	MethodRequestUserInput = "item/tool/requestUserInput"
	// Approval requests. Under approvalPolicy "never" these must not
	// arrive; if one does the adapter fails closed (declines) rather than
	// turning an approval into human input.
	MethodCommandApproval = "item/commandExecution/requestApproval"
	MethodFileApproval    = "item/fileChange/requestApproval"
	MethodPermissions     = "item/permissions/requestApproval"
)

// Server→client notification methods (verified subset).
const (
	NotifyThreadStarted   = "thread/started"
	NotifyTurnStarted     = "turn/started"
	NotifyTurnCompleted   = "turn/completed"
	NotifyItemStarted     = "item/started"
	NotifyItemCompleted   = "item/completed"
	NotifyAgentMessage    = "item/agentMessage/delta"
	NotifyTokenUsage      = "thread/tokenUsage/updated"
	NotifyRateLimits      = "account/rateLimits/updated"
	NotifyServerReqResolv = "serverRequest/resolved"
	NotifyError           = "error"
)

// ApprovalPolicy values (AskForApproval).
const (
	// ApprovalNever is the bypass policy Relay configures.
	ApprovalNever = "never"
)

// ClientInfo identifies Relay to the app-server during initialize.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeParams is the initialize handshake body.
type InitializeParams struct {
	ClientInfo ClientInfo `json:"clientInfo"`
}

// InitializeResult is the handshake response (verified fields).
type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOs     string `json:"platformOs"`
}

// UserInput is one turn input element. Only the verified text variant is
// used by Relay.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextInput builds a text input element.
func TextInput(text string) UserInput {
	return UserInput{
		Type: "text",
		Text: text,
	}
}

// ThreadStartParams is the verified thread/start body subset.
type ThreadStartParams struct {
	Model          string          `json:"model,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	ApprovalPolicy string          `json:"approvalPolicy,omitempty"`
	ServiceTier    string          `json:"serviceTier,omitempty"`
	Config         json.RawMessage `json:"config,omitempty"`
}

// ThreadResumeParams is the verified thread/resume body subset.
type ThreadResumeParams struct {
	ThreadID       string `json:"threadId"`
	Model          string `json:"model,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	ServiceTier    string `json:"serviceTier,omitempty"`
}

// Thread is the verified thread object subset.
type Thread struct {
	ID        string `json:"id"`
	CliVer    string `json:"cliVersion,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
}

// ThreadStartResult is the verified thread/start response subset.
type ThreadStartResult struct {
	Thread         Thread `json:"thread"`
	Model          string `json:"model,omitempty"`
	ModelProvider  string `json:"modelProvider,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	ServiceTier    string `json:"serviceTier,omitempty"`
}

// ThreadResumeResult is the verified thread/resume response subset.
type ThreadResumeResult struct {
	Thread         Thread `json:"thread"`
	Model          string `json:"model,omitempty"`
	ModelProvider  string `json:"modelProvider,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	ServiceTier    string `json:"serviceTier,omitempty"`
}

// TurnStartParams is the verified turn/start body subset.
type TurnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []UserInput `json:"input"`
	Model    string      `json:"model,omitempty"`
	Effort   string      `json:"effort,omitempty"`
	// ServiceTierForTurn is the per-turn service tier override.
	ServiceTierForTurn string `json:"serviceTierForTurn,omitempty"`
}

// Turn is the verified turn object subset.
type Turn struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"` // completed|interrupted|failed|inProgress
	Error       *TurnError   `json:"error,omitempty"`
	Items       []ThreadItem `json:"items,omitempty"`
	DurationMs  *int64       `json:"durationMs,omitempty"`
	CompletedAt *int64       `json:"completedAt,omitempty"`
}

// TurnError is the verified turn failure payload.
type TurnError struct {
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

// TurnStartResult is the verified turn/start response.
type TurnStartResult struct {
	Turn Turn `json:"turn"`
}

// TurnInterruptParams is the verified turn/interrupt body.
type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// ThreadItem is the verified thread item subset (type discriminator plus
// the fields Relay reads). The native item union is far richer; Relay
// decodes only the variants it maps to canonical/telemetry events and
// ignores the rest rather than guessing.
type ThreadItem struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	// commandExecution item fields.
	Command          string          `json:"command,omitempty"`
	CommandActions   []CommandAction `json:"commandActions,omitempty"`
	AggregatedOutput string          `json:"aggregatedOutput,omitempty"`
	ExitCode         *int64          `json:"exitCode,omitempty"`
	Status           string          `json:"status,omitempty"`
	DurationMs       *int64          `json:"durationMs,omitempty"`
	// fileChange item fields.
	Changes []FileUpdateChange `json:"changes,omitempty"`
	// mcpToolCall / dynamicToolCall / webSearch / functionCallOutput fields.
	Tool      string          `json:"tool,omitempty"`
	Server    string          `json:"server,omitempty"`
	Name      string          `json:"name,omitempty"`
	Query     string          `json:"query,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Success   *bool           `json:"success,omitempty"`
	ItemError json.RawMessage `json:"error,omitempty"`
}

// CommandAction is Codex's structured classification of what a command
// actually did (verified union: read|listFiles|search|unknown). A "read"
// action is objective source-delivery evidence — not a guessed `cat`.
type CommandAction struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Query   string `json:"query,omitempty"`
}

// FileUpdateChange is one fileChange entry: a path plus the exact diff
// content present in the item.
type FileUpdateChange struct {
	Path string          `json:"path"`
	Diff string          `json:"diff"`
	Kind json.RawMessage `json:"kind,omitempty"`
}

// AgentMessageDeltaNotification is the verified delta notification.
type AgentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// TurnCompletedNotification is the verified completion notification.
type TurnCompletedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

// ItemStartedNotification is the verified item-start notification.
type ItemStartedNotification struct {
	ThreadID    string     `json:"threadId"`
	TurnID      string     `json:"turnId"`
	Item        ThreadItem `json:"item"`
	StartedAtMs int64      `json:"startedAtMs"`
}

// ItemCompletedNotification is the verified item completion notification.
type ItemCompletedNotification struct {
	ThreadID      string     `json:"threadId"`
	TurnID        string     `json:"turnId"`
	Item          ThreadItem `json:"item"`
	CompletedAtMs int64      `json:"completedAtMs"`
}

// ToolRequestUserInputParams is the verified requested-input payload
// (EXPERIMENTAL in the schema, shipped by the installed CLI).
type ToolRequestUserInputParams struct {
	ThreadID   string                     `json:"threadId"`
	TurnID     string                     `json:"turnId"`
	ItemID     string                     `json:"itemId"`
	IsBlocking bool                       `json:"isBlocking"`
	Questions  []ToolRequestUserInputItem `json:"questions"`
}

// ToolRequestUserInputItem is one requested-input question.
type ToolRequestUserInputItem struct {
	ID       string                    `json:"id"`
	Header   string                    `json:"header"`
	Question string                    `json:"question"`
	Options  []ToolRequestUserInputOpt `json:"options,omitempty"`
	IsOther  bool                      `json:"isOther,omitempty"`
	IsSecret bool                      `json:"isSecret,omitempty"`
}

// ToolRequestUserInputOpt is one selectable option.
type ToolRequestUserInputOpt struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ToolRequestUserInputAnswer is the answer for one question.
type ToolRequestUserInputAnswer struct {
	Answers []string `json:"answers"`
}

// ToolRequestUserInputResponse maps question IDs to answers.
type ToolRequestUserInputResponse struct {
	Answers map[string]ToolRequestUserInputAnswer `json:"answers"`
}

// TokenUsageBreakdown is the verified token accounting shape.
type TokenUsageBreakdown struct {
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
	CacheWriteInputTkns   int64 `json:"cacheWriteInputTokens,omitempty"`
}

// ThreadTokenUsage is the verified per-thread usage shape.
type ThreadTokenUsage struct {
	Last               TokenUsageBreakdown `json:"last"`
	Total              TokenUsageBreakdown `json:"total"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitempty"`
}

// ThreadTokenUsageUpdatedNotification is the verified usage notification.
type ThreadTokenUsageUpdatedNotification struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

// RateLimitWindow is one rate-limit window (verified fields).
type RateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent,omitempty"`
	WindowMinutes      *int64   `json:"windowMinutes,omitempty"`
	ResetsInSeconds    *int64   `json:"resetsInSeconds,omitempty"`
	ResetsAtUnixSecond *int64   `json:"resetsAt,omitempty"`
}

// RateLimitSnapshot is the verified rate-limit shape subset.
type RateLimitSnapshot struct {
	LimitID            *string          `json:"limitId,omitempty"`
	LimitName          *string          `json:"limitName,omitempty"`
	Primary            *RateLimitWindow `json:"primary,omitempty"`
	Secondary          *RateLimitWindow `json:"secondary,omitempty"`
	RateLimitReachedTy *string          `json:"rateLimitReachedType,omitempty"`
	PlanType           *string          `json:"planType,omitempty"`
}

// AccountRateLimitsUpdatedNotification is the verified rate-limit
// notification.
type AccountRateLimitsUpdatedNotification struct {
	RateLimits RateLimitSnapshot `json:"rateLimits"`
}
