package events

// ToolChoice controls which tools the LLM is allowed or required to use.
type ToolChoice struct {
	Mode string `json:"mode"`           // "auto" | "required" | "none" | "tool"
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
	Prediction     string // OpenAI-only: known-content prediction for low-latency edits.
	// Other providers ignore this field. Empty = no prediction.
	Metadata map[string]any
}

// Message represents a single message in an LLM conversation.
type Message struct {
	Role    string // "system", "user", "assistant", "tool"
	Content string
	Parts   []MessagePart // optional multimodal parts; when non-empty, providers
	// that support multimodal serialize these alongside or instead of Content.
	// Providers without multimodal support fall back to Content (text-only path).
	ToolCallID string            // for tool result messages
	ToolCalls  []ToolCallRequest // for assistant messages with tool calls

	// Metadata carries provider-specific round-trip data that must survive a
	// turn boundary. Example: Anthropic extended-thinking emits opaque
	// `thinking` and `redacted_thinking` blocks (with cryptographic signatures)
	// that MUST be echoed back verbatim on the next assistant turn after a
	// tool result, otherwise the API rejects the request with HTTP 400.
	// Providers stash these as Metadata["thinking_blocks"] on the assistant
	// Message and read them back when serializing the next request body.
	// Keys are namespaced informally per-provider; consumers other than the
	// owning provider should treat this map as opaque.
	Metadata map[string]any
}

// MessagePart carries a single piece of multimodal content. Providers that
// don't support a given Type should skip the part (or concatenate Text parts).
// Either Data (inline bytes) or URI (provider-hosted reference) is set, never
// both. Inline payloads beyond a provider-specific size limit are uploaded by
// the provider and replaced with a URI on send.
type MessagePart struct {
	Type     string // "text" | "image" | "audio" | "video" | "file"
	Text     string // when Type == "text"
	MimeType string // e.g. "image/png", "application/pdf"; required when Data or URI set
	Data     []byte // inline bytes
	URI      string // provider-hosted reference (e.g. Gemini Files API URI)
	FileID   string // provider-issued file id (e.g. Anthropic file_..., OpenAI file-...).
	// When set, providers reference the file by id rather than re-uploading or
	// inlining bytes. Takes precedence over Data if both are set; takes
	// precedence over URI for providers that have a native file_id source type
	// (Anthropic Files API, OpenAI plan 14). Cross-provider field — Anthropic
	// owns it first, OpenAI will reuse on plan 14.
}

// ToolCallRequest represents a tool invocation requested by the LLM.
type ToolCallRequest struct {
	ID        string
	Name      string
	Arguments string // JSON string
}

// ToolDef describes a tool available to the LLM.
type ToolDef struct {
	Name         string
	Description  string
	Parameters   map[string]any // JSON Schema for inputs
	OutputSchema map[string]any // Optional JSON Schema for structured outputs. When
	// set, the tool commits to populating ToolResult.OutputStructured with a
	// value that validates against this schema. Consumers like run_code use
	// it to generate typed bindings; omit for tools that only produce text.
	Class    string   // Semantic class (e.g. "filesystem", "memory"). Empty = classless.
	Subclass string   // Optional grouping within class (e.g. "read", "write").
	Tags     []string // Cross-cutting metadata for filtering.
}

// LLMResponse is the complete response from a language model.
type LLMResponse struct {
	Content      string
	ToolCalls    []ToolCallRequest
	Usage        Usage
	CostUSD      float64 // provider-computed cost for this request
	Model        string
	FinishReason string
	Metadata     map[string]any

	// Citations carries native source attributions emitted by providers that
	// support them (currently Anthropic). Nil when the provider doesn't emit
	// citations or the request didn't include citation-enabled documents.
	Citations []Citation

	// Alternatives holds additional responses from parallel fanout providers.
	// Nil for normal (non-fanout) requests. When populated, the primary fields
	// above contain the first successful response (or selected winner), and
	// Alternatives contains the remaining responses.
	Alternatives []LLMResponse
}

// Citation is a single source attribution returned by a provider that supports
// native citation tracking (currently Anthropic). Other providers leave the
// LLMResponse.Citations slice nil.
type Citation struct {
	Type            string // "char_location" | "page_location" | "content_block_location"
	CitedText       string
	DocumentIndex   int
	DocumentTitle   string
	StartCharIndex  int
	EndCharIndex    int
	StartPageNumber int
	EndPageNumber   int
	StartBlockIndex int
	EndBlockIndex   int
}

// Usage tracks token consumption for an LLM call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int // thinking/reasoning tokens (Gemini 2.5 thoughtTokenCount, etc.)
	CachedTokens     int // tokens served from a prompt cache (billed at a discount)
	CacheWriteTokens int // tokens written into a prompt cache (billed at a premium over plain input)
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

// BatchSubmit is published by callers (CLI subcommand, agent tool, etc.) to
// request that a slice of LLMRequests be sent to a provider's batch endpoint
// rather than the synchronous llm.request path. The coordinator plugin
// (`nexus.llm.batch`) handles dispatch, polling, and result aggregation.
//
// Bus event type: "llm.batch.submit".
type BatchSubmit struct {
	Provider string // "anthropic" | "openai"
	Requests []BatchRequest
	Metadata map[string]any
}

// BatchRequest pairs a stable caller-defined id with an LLMRequest. The
// CustomID is how the caller correlates results (per-request) when the
// batch returns out of order.
type BatchRequest struct {
	CustomID string
	Request  LLMRequest
}

// BatchStatus is emitted periodically by the coordinator while the batch is
// in flight. Counts are best-effort — providers report different shapes.
//
// Bus event type: "llm.batch.status".
type BatchStatus struct {
	Provider string
	BatchID  string
	Status   string // "submitted" | "in_progress" | "completed" | "failed" | "cancelled"
	Counts   BatchCounts
}

// BatchCounts is a best-effort snapshot of per-request progress in a batch.
type BatchCounts struct {
	Total     int
	Completed int
	Failed    int
}

// BatchResults is emitted once at the end of a batch, carrying all per-request
// results.
//
// Bus event type: "llm.batch.results".
type BatchResults struct {
	Provider string
	BatchID  string
	Results  []BatchResult
}

// BatchResult is a single per-request entry in a BatchResults payload. Either
// Response is non-nil (success) or Error is non-empty (this individual request
// failed); callers must check both.
type BatchResult struct {
	CustomID string
	Response *LLMResponse // nil when Error is set
	Error    string       // non-empty when this individual request failed
}
