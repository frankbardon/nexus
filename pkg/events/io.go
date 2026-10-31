package events

import "time"

// Schema-version constants for io.* payloads. See doc.go.
const (
	UserInputVersion        = 3
	AgentOutputVersion      = 1
	OutputChunkVersion      = 1
	StreamRefVersion        = 1
	StatusUpdateVersion     = 1
	ApprovalRequestVersion  = 1
	ApprovalResponseVersion = 1
	HistoryReplayVersion    = 1
	SessionInfoVersion      = 1
)

// UserInput represents input submitted by the user.
//
// SchemaVersion bumped to 2 when PreloadMessages was added, and to 3 when
// Headers was. Producers that only set the older fields remain forward-
// compatible: every added field is an optional map or slice, so a v1 or v2
// emitter still parses cleanly into the v3 struct (the newer fields are nil).
// Consumers that emit fresh v3 payloads stamp UserInputVersion.
type UserInput struct {
	SchemaVersion int `json:"_schema_version"`

	Content   string
	Files     []FileAttachment
	SessionID string

	// PreloadMessages, when non-empty, are appended to memory.history in
	// order before Content. Each message keeps its declared Role so
	// assistant-role priming, few-shot examples, or recorded transcripts
	// can be replayed without abusing the user channel. Multimodal parts
	// ride on Message.Parts. Memory plugins consume PreloadMessages first,
	// then fall through to the existing Content append + agent trigger.
	//
	// Added in v2 for MCP prompt expansion; also useful for scripted
	// oneshot IO and replay.
	PreloadMessages []Message

	// Headers carries the X-Nexus-* request headers of the HTTP request that
	// opened this turn, keyed by normalized name (`X-Nexus-Tenant-ID` ->
	// `tenant-id`). See pkg/nexusheaders for the convention and the bounds,
	// and engine.SessionWorkspace.SetRequestHeaders for the parallel binding
	// every web transport performs before emitting this event.
	//
	// Nil for a transport with no HTTP request behind it (TUI, oneshot)
	// and for a web request that carried no X-Nexus-* header. A handler must
	// treat it as a hint from the caller, never as an authenticated claim:
	// nothing verifies these values, which is why the engine binds them into
	// the reserved, prompt-invisible label namespace rather than the general
	// one.
	//
	// Added in v3.
	Headers map[string]string
}

// FileAttachment is a file attached to user input.
type FileAttachment struct {
	Name     string
	MimeType string
	Data     []byte
}

// AgentOutput represents a complete output message from the agent.
type AgentOutput struct {
	SchemaVersion int `json:"_schema_version"`

	Content  string
	Role     string // "assistant", "system", "tool"
	Metadata map[string]any
	TurnID   string
}

// OutputChunk represents a single chunk of streamed output.
type OutputChunk struct {
	SchemaVersion int `json:"_schema_version"`

	Content string
	TurnID  string
	Index   int
}

// StreamRef marks the start of a streaming response.
type StreamRef struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID   string
	Metadata map[string]any
}

// StatusUpdate describes the current operational state of the agent.
type StatusUpdate struct {
	SchemaVersion int `json:"_schema_version"`

	State  string // "idle", "thinking", "tool_running", "streaming"
	Detail string
	ToolID string
}

// ApprovalRequest asks the user to approve an action.
type ApprovalRequest struct {
	SchemaVersion int `json:"_schema_version"`

	PromptID    string
	Description string
	ToolCall    string
	Risk        string // "low", "medium", "high"
}

// ApprovalResponse carries the user's approval decision.
type ApprovalResponse struct {
	SchemaVersion int `json:"_schema_version"`

	PromptID string
	Approved bool
	Always   bool
}

// HistoryReplay carries persisted conversation messages for UI display on session recall.
type HistoryReplay struct {
	SchemaVersion int `json:"_schema_version"`

	Messages []Message
}

// SessionInfo describes a connected session.
type SessionInfo struct {
	SchemaVersion int `json:"_schema_version"`

	ID          string
	Transport   string
	ConnectedAt time.Time
	UserAgent   string
}
