package chat

// Wire types for the subset of the Agent Client Protocol (https://agentclientprotocol.com)
// the ACP engine uses. Unknown fields are ignored; agent-specific extensions live in _meta.

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

type acpPresence struct{ set bool }

func (p *acpPresence) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	v, err := dec.ReadValue()
	if err != nil {
		return err
	}
	k := v.Kind()
	p.set = k != 'n' && k != 'f'
	return nil
}

type acpInitResult struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession        bool `json:"loadSession"`
		PromptCapabilities struct {
			Image           bool `json:"image"`
			Audio           bool `json:"audio"`
			EmbeddedContext bool `json:"embeddedContext"`
		} `json:"promptCapabilities"`
		MCPCapabilities struct {
			HTTP bool `json:"http"`
			SSE  bool `json:"sse"`
		} `json:"mcpCapabilities"`
		SessionCapabilities struct {
			Resume acpPresence `json:"resume"`
			Close  acpPresence `json:"close"`
			Load   acpPresence `json:"load"`
			List   acpPresence `json:"list"`
		} `json:"sessionCapabilities"`
		Meta map[string]jsontext.Value `json:"_meta"`
	} `json:"agentCapabilities"`
	AgentInfo *struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	AuthMethods []acpAuthMethod           `json:"authMethods"`
	Meta        map[string]jsontext.Value `json:"_meta"`
}

func (r *acpInitResult) version() string {
	if r.AgentInfo != nil && r.AgentInfo.Version != "" {
		return r.AgentInfo.Version
	}
	var v string
	if raw, ok := r.Meta["agentVersion"]; ok {
		_ = unmarshal(raw, &v)
	}
	return v
}

// systemPromptAppend reports whether the agent accepts _meta.antigravity.systemPrompt.append
// on session/new (agy advertises agentCapabilities._meta.antigravity.systemPrompt.append).
func (r *acpInitResult) systemPromptAppend() bool {
	raw, ok := r.AgentCapabilities.Meta["antigravity"]
	if !ok {
		return false
	}
	var ag struct {
		SystemPrompt struct {
			Append bool `json:"append"`
		} `json:"systemPrompt"`
	}
	return unmarshal(raw, &ag) == nil && ag.SystemPrompt.Append
}

type acpAuthMethod struct {
	ID          string                    `json:"id"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Meta        map[string]jsontext.Value `json:"_meta"`
}

// terminalAuth returns the login command an agent advertises via _meta["terminal-auth"]
// (copilot: {command, args, label}).
func (m acpAuthMethod) terminalAuth() (string, bool) {
	raw, ok := m.Meta["terminal-auth"]
	if !ok {
		return "", false
	}
	var ta struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if unmarshal(raw, &ta) != nil || ta.Command == "" {
		return "", false
	}
	return strings.TrimSpace(ta.Command + " " + strings.Join(ta.Args, " ")), true
}

type acpSessionResult struct {
	SessionID     string            `json:"sessionId"`
	Models        *acpModelState    `json:"models"`
	Modes         *acpModeState     `json:"modes"`
	ConfigOptions []acpConfigOption `json:"configOptions"`
	Meta          jsontext.Value    `json:"_meta"`
}

type acpModelState struct {
	CurrentModelID  string         `json:"currentModelId"`
	AvailableModels []acpModelInfo `json:"availableModels"`
}

type acpModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Meta        struct {
		TotalContextTokens int64  `json:"totalContextTokens"`
		ReasoningEffort    string `json:"reasoningEffort"`
		ReasoningEfforts   []struct {
			ID          string `json:"id"`
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
			Default     bool   `json:"default"`
		} `json:"reasoningEfforts"`
	} `json:"_meta"`
}

type acpModeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"availableModes"`
}

type acpConfigOption struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Category     string           `json:"category"`
	Type         string           `json:"type"`
	CurrentValue jsontext.Value   `json:"currentValue"`
	Options      []acpConfigValue `json:"options"`
}

type acpConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UnmarshalJSONFrom flattens grouped select options ({group, name, options:[...]}).
func (o *acpConfigOption) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type plain acpConfigOption
	var aux struct {
		plain
		Options []jsontext.Value `json:"options"`
	}
	if err := json.UnmarshalDecode(dec, &aux); err != nil {
		return err
	}
	*o = acpConfigOption(aux.plain)
	o.Options = nil
	for _, raw := range aux.Options {
		var g struct {
			acpConfigValue
			Options []acpConfigValue `json:"options"`
		}
		if unmarshal(raw, &g) != nil {
			continue
		}
		if len(g.Options) > 0 {
			o.Options = append(o.Options, g.Options...)
		} else if g.Value != "" {
			o.Options = append(o.Options, g.acpConfigValue)
		}
	}
	return nil
}

func (o acpConfigOption) current() string {
	var s string
	if unmarshal(o.CurrentValue, &s) == nil {
		return s
	}
	return strings.Trim(string(o.CurrentValue), `"`)
}

func (o acpConfigOption) has(v string) bool {
	for _, x := range o.Options {
		if x.Value == v {
			return true
		}
	}
	return false
}

// findConfig returns the config option with the given id or category.
func findConfig(opts []acpConfigOption, idOrCategory ...string) (acpConfigOption, bool) {
	for _, want := range idOrCategory {
		for _, o := range opts {
			if o.ID == want {
				return o, true
			}
		}
		for _, o := range opts {
			if o.Category == want {
				return o, true
			}
		}
	}
	return acpConfigOption{}, false
}

// --- session/update -------------------------------------------------------------------

type acpUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Update    jsontext.Value `json:"update"`
}

type acpUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`

	// agent_message_chunk / agent_thought_chunk / user_message_chunk
	Content jsontext.Value `json:"content"`
	// tool_call / tool_call_update
	ToolCallID string         `json:"toolCallId"`
	Title      *string        `json:"title"`
	Kind       *string        `json:"kind"`
	Status     *string        `json:"status"`
	RawInput   jsontext.Value `json:"rawInput"`
	RawOutput  jsontext.Value `json:"rawOutput"`
	Locations  []struct {
		Path string `json:"path"`
		Line *int   `json:"line"`
	} `json:"locations"`
	// plan
	Entries []struct {
		Content  string `json:"content"`
		Priority string `json:"priority"`
		Status   string `json:"status"`
	} `json:"entries"`
	// current_mode_update
	CurrentModeID string `json:"currentModeId"`
	// config_option_update
	ConfigOptions []acpConfigOption `json:"configOptions"`
	// available_commands_update
	AvailableCommands []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"availableCommands"`
	Meta map[string]jsontext.Value `json:"_meta"`
}

// acpContentBlock is one ContentBlock (text, image, resource, resource_link) or one
// ToolCallContent (content, diff, terminal).
type acpContentBlock struct {
	Type    string           `json:"type"`
	Text    string           `json:"text"`
	Content *acpContentBlock `json:"content"` // ToolCallContent{type:"content"}
	Path    string           `json:"path"`    // diff
	OldText *string          `json:"oldText"`
	NewText string           `json:"newText"`
	URI     string           `json:"uri"`
	Name    string           `json:"name"`
}

type acpPermissionParams struct {
	SessionID string         `json:"sessionId"`
	ToolCall  jsontext.Value `json:"toolCall"`
	Options   []struct {
		OptionID string `json:"optionId"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

type acpPromptResult struct {
	StopReason string `json:"stopReason"`
	Usage      *struct {
		InputTokens       int64 `json:"inputTokens"`
		OutputTokens      int64 `json:"outputTokens"`
		CachedReadTokens  int64 `json:"cachedReadTokens"`
		CachedWriteTokens int64 `json:"cachedWriteTokens"`
	} `json:"usage"`
}
