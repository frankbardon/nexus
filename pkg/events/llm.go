package events

// ToolChoice controls which tools the LLM is allowed or required to use.
type ToolChoice struct {
	Mode string `json:"mode"` // "auto" | "required" | "none" | "tool"
	Name string `json:"name,omitempty"` // tool name when Mode == "tool"
}

// ToolFilter restricts which tools are available for an LLM request.
// Include and Exclude are mutually exclusive; Include takes precedence.
type ToolFilter struct {
	Include []string `json:"include,omitempty"` // only these tools (empty = all)
	Exclude []string `json:"exclude,omitempty"` // remove these tools
}

// ResponseFormat specifies the desired output format for an LLM request.
// Providers map this to their native structured output mechanism when supported,
// or simulate via tool-use-as-schema when not.
type ResponseFormat struct {
	Type   string         `json:"type"`             // "text" | "json_object" | "json_schema"
	Name   string         `json:"name,omitempty"`   // schema name (OpenAI requires this)
	Schema map[string]any `json:"schema,omitempty"` // JSON Schema
	Strict bool           `json:"strict,omitempty"` // enforce strict schema adherence
}

// LLMRequest describes a request to a language model.
type LLMRequest struct {
	Role           string // Model role (e.g., "reasoning", "balanced", "quick")
	Model          string // Explicit model ID override (takes precedence over Role)
	Messages       []Message
	Tools          []ToolDef
	ToolChoice     *ToolChoice     // nil = provider default (auto)
	ToolFilter     *ToolFilter     // nil = no filtering
	ResponseFormat *ResponseFormat // nil = no structured output constraint
	MaxTokens      int
	Temperature    *float64
	Stream         bool
	Metadata       map[string]any
}

// Message represents a single message in an LLM conversation.
type Message struct {
	Role       string // "system", "user", "assistant", "tool"
	Content    string
	ToolCallID string            // for tool result messages
	ToolCalls  []ToolCallRequest // for assistant messages with tool calls
}

// ToolCallRequest represents a tool invocation requested by the LLM.
type ToolCallRequest struct {
	ID        string
	Name      string
	Arguments string // JSON string
}

// ToolDef describes a tool available to the LLM.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema
}

// LLMResponse is the complete response from a language model.
type LLMResponse struct {
	Content      string
	ToolCalls    []ToolCallRequest
	Usage        Usage
	Model        string
	FinishReason string
	Metadata     map[string]any
}

// Usage tracks token consumption for an LLM call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// StreamChunk is a single chunk from a streaming LLM response.
type StreamChunk struct {
	Content  string
	ToolCall *ToolCallRequest // partial tool call
	Index    int
	TurnID   string
}

// StreamEnd signals the completion of a streaming LLM response.
type StreamEnd struct {
	TurnID       string
	Usage        Usage
	FinishReason string
}
