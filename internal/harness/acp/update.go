package acp

import "encoding/json"

// ConfigOption is one advertised configuration option. The runtime owns the
// option IDs and categories; Relay discovers them and never hard-codes an ID
// it could read from the protocol.
type ConfigOption struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Category     string          `json:"category,omitempty"`
	Type         string          `json:"type"`
	CurrentValue json.RawMessage `json:"currentValue,omitempty"`
	Options      json.RawMessage `json:"options,omitempty"`
}

// Config option categories Relay recognizes (the verified enum plus vendor
// extensions; an unknown category is ignored, never guessed at).
const (
	// CategoryModel is the model selector.
	CategoryModel = "model"
	// CategoryMode is the session mode selector.
	CategoryMode = "mode"
	// CategoryThoughtLevel is the reasoning-effort selector.
	CategoryThoughtLevel = "thought_level"
)

// Config option types.
const (
	// TypeSelect is a value chosen from the advertised options.
	TypeSelect = "select"
	// TypeBoolean is an on/off option.
	TypeBoolean = "boolean"
)

// ConfigSelectOption is one selectable value.
type ConfigSelectOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ConfigSelectGroup is a named group of selectable values; a select option's
// option list is either flat or grouped.
type ConfigSelectGroup struct {
	Group   string               `json:"group"`
	Name    string               `json:"name"`
	Options []ConfigSelectOption `json:"options"`
}

// SelectOptions returns the selectable values, flattening groups. An
// unreadable option list yields no values (never an invented one).
func (o ConfigOption) SelectOptions() []ConfigSelectOption {
	if len(o.Options) == 0 {
		return nil
	}
	var flat []ConfigSelectOption
	if err := json.Unmarshal(o.Options, &flat); err == nil {
		return flat
	}
	var groups []ConfigSelectGroup
	if err := json.Unmarshal(o.Options, &groups); err == nil {
		out := make([]ConfigSelectOption, 0, len(groups))
		for _, g := range groups {
			out = append(out, g.Options...)
		}
		return out
	}
	return nil
}

// CurrentString returns the option's current value when it is a string.
func (o ConfigOption) CurrentString() (string, bool) {
	var v string
	if len(o.CurrentValue) == 0 || json.Unmarshal(o.CurrentValue, &v) != nil {
		return "", false
	}
	return v, true
}

// CurrentBool returns the option's current value when it is a boolean.
func (o ConfigOption) CurrentBool() (bool, bool) {
	var v bool
	if len(o.CurrentValue) == 0 || json.Unmarshal(o.CurrentValue, &v) != nil {
		return false, false
	}
	return v, true
}

// FindOption returns the first advertised option matching a category, then
// (as a fallback) one matching the given ID. Discovery is preferred over a
// hard-coded ID, but an ID match is honored when the runtime does not
// categorize.
func FindOption(options []ConfigOption, category, id string) (ConfigOption, bool) {
	for _, o := range options {
		if category != "" && o.Category == category {
			return o, true
		}
	}
	for _, o := range options {
		if id != "" && o.ID == id {
			return o, true
		}
	}
	return ConfigOption{}, false
}

// SetConfigOptionParams is the `session/set_config_option` request. Value is
// a string for a select option and a boolean for a boolean option; the
// boolean form also carries the explicit type.
type SetConfigOptionParams struct {
	ConfigID  string `json:"configId"`
	SessionID string `json:"sessionId"`
	Value     any    `json:"value"`
	Type      string `json:"type,omitempty"`
}

// StringValue builds a select-option value.
func StringValue(v string) SetConfigOptionParams {
	return SetConfigOptionParams{Value: v}
}

// BoolValue builds a boolean-option value.
func BoolValue(v bool) SetConfigOptionParams {
	return SetConfigOptionParams{
		Value: v,
		Type:  TypeBoolean,
	}
}

// SetConfigOptionResult is the `session/set_config_option` response: the
// updated option set.
type SetConfigOptionResult struct {
	ConfigOptions []ConfigOption `json:"configOptions"`
}

// SetModeParams is the `session/set_mode` request.
type SetModeParams struct {
	ModeID    string `json:"modeId"`
	SessionID string `json:"sessionId"`
}

// SessionUpdateNotification is the agent→client `session/update` payload.
type SessionUpdateNotification struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

// Update is the flattened session/update union. Relay reads only the
// variants it maps to canonical events; an unknown variant is ignored.
type Update struct {
	SessionUpdate string        `json:"sessionUpdate"`
	Content       *ContentBlock `json:"content,omitempty"`
	MessageID     string        `json:"messageId,omitempty"`
	// usage_update: the native context window accounting.
	Cost *float64 `json:"cost,omitempty"`
	Size *int64   `json:"size,omitempty"`
	Used *int64   `json:"used,omitempty"`
	// session_info_update.
	Title     *string `json:"title,omitempty"`
	UpdatedAt *string `json:"updatedAt,omitempty"`
	// config_option_update.
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
	// current_mode_update.
	CurrentModeID string `json:"currentModeId,omitempty"`
}

// Text returns the update's text content, when it carries any.
func (u Update) Text() string {
	if u.Content == nil {
		return ""
	}
	return u.Content.Text
}
