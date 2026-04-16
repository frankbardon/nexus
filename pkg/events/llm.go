package events

// LLMRequest describes a request to a language model.
type LLMRequest struct {
	Role        string // Model role (e.g., "reasoning", "balanced", "quick")
	Model       string // Explicit model ID override (takes precedence over Role)
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature *float64
	Stream      bool
	Metadata    map[string]any
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
