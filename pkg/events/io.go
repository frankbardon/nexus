package events

import "time"

// Schema-version constants for io.* payloads. See doc.go.
const (
	UserInputVersion             = 1
	AgentOutputVersion           = 1
	OutputChunkVersion           = 1
	StreamRefVersion             = 1
	StatusUpdateVersion          = 1
	ApprovalRequestVersion       = 1
	ApprovalResponseVersion      = 1
	HistoryReplayVersion         = 1
	FileOpenRequestVersion       = 1
	FileOpenResponseVersion      = 1
	FileOutputDirRequestVersion  = 1
	FileOutputDirResponseVersion = 1
	FileSelectedVersion          = 1
	SessionInfoVersion           = 1
)

// UserInput represents input submitted by the user.
type UserInput struct {
	SchemaVersion int `json:"_schema_version"`

	Content   string
	Files     []FileAttachment
	SessionID string
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

// FileOpenRequest asks the IO shell to present an OS-native file open
// dialog. This is a wrapper-feature event (CLAUDE.md §7a): only
// nexus.io.wails handles it. nexus.io.browser deliberately ignores it —
// a session-scoped browser transport has no business popping native
// dialogs on the host machine.
//
// Callers emit FileOpenRequest and subscribe to FileOpenResponse
// themselves, correlating on RequestID. The handler runs the dialog on
// a background goroutine so the bus is never held by the native UI.
type FileOpenRequest struct {
	SchemaVersion int `json:"_schema_version"`

	RequestID        string
	Title            string
	DefaultDirectory string
	Filters          []FileFilter
}

// FileFilter describes a single entry in the file dialog's type filter
// dropdown. Pattern follows Wails conventions: semicolon-separated
// glob patterns, e.g. "*.pdf;*.PDF".
type FileFilter struct {
	DisplayName string
	Pattern     string
}

// FileOpenResponse carries the outcome of a FileOpenRequest.
//
// Exactly one of Path, Cancelled, or Error will be meaningful:
//   - Path set, Cancelled false, Error empty: user picked a file.
//   - Path empty, Cancelled true: user closed the dialog without picking.
//   - Error non-empty: dialog failed to run (no runtime attached, OS
//     refused, etc.). Treat this the same as cancelled from a UX
//     perspective, but log it.
type FileOpenResponse struct {
	SchemaVersion int `json:"_schema_version"`

	RequestID string
	Path      string
	Cancelled bool
	Error     string
}

// FileOutputDirRequest asks the shell to return the configured output
// directory for the requesting agent. The shell responds with
// FileOutputDirResponse, correlated on RequestID.
type FileOutputDirRequest struct {
	SchemaVersion int `json:"_schema_version"`

	RequestID string
}

// FileOutputDirResponse carries the resolved output directory path.
type FileOutputDirResponse struct {
	SchemaVersion int `json:"_schema_version"`

	RequestID string
	Path      string
	Error     string
}

// FileSelected is emitted when the user selects a file in the file
// browser panel. Agent plugins can subscribe to this event to react
// to file selection without any Wails awareness.
type FileSelected struct {
	SchemaVersion int `json:"_schema_version"`

	Path string
	Name string
	Size int64
}

// SessionInfo describes a connected session.
type SessionInfo struct {
	SchemaVersion int `json:"_schema_version"`

	ID          string
	Transport   string
	ConnectedAt time.Time
	UserAgent   string
}
