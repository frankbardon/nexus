package events

import "time"

// UserInput represents input submitted by the user.
type UserInput struct {
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
	Content  string
	Role     string // "assistant", "system", "tool"
	Metadata map[string]any
	TurnID   string
}

// OutputChunk represents a single chunk of streamed output.
type OutputChunk struct {
	Content string
	TurnID  string
	Index   int
}

// StreamRef marks the start of a streaming response.
type StreamRef struct {
	TurnID   string
	Metadata map[string]any
}

// StatusUpdate describes the current operational state of the agent.
type StatusUpdate struct {
	State  string // "idle", "thinking", "tool_running", "streaming"
	Detail string
	ToolID string
}

// ApprovalRequest asks the user to approve an action.
type ApprovalRequest struct {
	PromptID    string
	Description string
	ToolCall    string
	Risk        string // "low", "medium", "high"
}

// ApprovalResponse carries the user's approval decision.
type ApprovalResponse struct {
	PromptID string
	Approved bool
	Always   bool
}

// HistoryReplay carries persisted conversation messages for UI display on session recall.
type HistoryReplay struct {
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
	RequestID string
	Path      string
	Cancelled bool
	Error     string
}

// FileOutputDirRequest asks the shell to return the configured output
// directory for the requesting agent. The shell responds with
// FileOutputDirResponse, correlated on RequestID.
type FileOutputDirRequest struct {
	RequestID string
}

// FileOutputDirResponse carries the resolved output directory path.
type FileOutputDirResponse struct {
	RequestID string
	Path      string
	Error     string
}

// FileSelected is emitted when the user selects a file in the file
// browser panel. Agent plugins can subscribe to this event to react
// to file selection without any Wails awareness.
type FileSelected struct {
	Path string
	Name string
	Size int64
}

// SessionInfo describes a connected session.
type SessionInfo struct {
	ID          string
	Transport   string
	ConnectedAt time.Time
	UserAgent   string
}
