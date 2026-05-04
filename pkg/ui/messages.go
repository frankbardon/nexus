package ui

import (
	"encoding/json"
	"time"
)

// Envelope is the wire format for all messages over the UI transport.
type Envelope struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// OutputMessage carries a complete agent output to the UI.
type OutputMessage struct {
	Content  string         `json:"content"`
	Role     string         `json:"role"`
	Metadata map[string]any `json:"metadata"`
	TurnID   string         `json:"turn_id"`
}

// StreamChunkMessage carries a single chunk of streamed output.
type StreamChunkMessage struct {
	Content string `json:"content"`
	TurnID  string `json:"turn_id"`
	Index   int    `json:"index"`
}

// StreamEndMessage signals the end of a streaming response.
type StreamEndMessage struct {
	TurnID   string         `json:"turn_id"`
	Metadata map[string]any `json:"metadata"`
}

// StatusMessage conveys the agent's current operational state.
type StatusMessage struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
	ToolID string `json:"tool_id"`
}

// ApprovalRequestMessage asks the user to approve an action.
type ApprovalRequestMessage struct {
	PromptID    string `json:"prompt_id"`
	Description string `json:"description"`
	ToolCall    string `json:"tool_call"`
	Risk        string `json:"risk"`
}

// InputMessage carries user input from the UI.
type InputMessage struct {
	Content string           `json:"content"`
	Files   []FileAttachment `json:"files"`
}

// ApprovalResponseMessage carries the user's approval decision.
type ApprovalResponseMessage struct {
	PromptID string `json:"prompt_id"`
	Approved bool   `json:"approved"`
	Always   bool   `json:"always"`
}

// FileAttachment is a file attached to user input.
type FileAttachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// HITLChoiceMessage is one option presented in a multi-choice human-in-the-loop request.
type HITLChoiceMessage struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// HITLRequestMessage is the IO-facing payload for a hitl.requested event.
// IO plugins render Prompt and (when present) Choices, then return a
// HITLResponseMessage carrying the operator's pick or freeform answer.
type HITLRequestMessage struct {
	RequestID string              `json:"request_id"`
	Prompt    string              `json:"prompt"`
	Mode      string              `json:"mode"`
	Choices   []HITLChoiceMessage `json:"choices,omitempty"`
	TurnID    string              `json:"turn_id,omitempty"`
}

// HITLResponseMessage carries the operator's reply.
type HITLResponseMessage struct {
	RequestID string `json:"request_id"`
	ChoiceID  string `json:"choice_id,omitempty"`
	FreeText  string `json:"free_text,omitempty"`
}

// CodeExecStdoutMessage streams a chunk of stdout from a run_code script
// while it is still executing. CallID keys the message so IO plugins can
// render each script as its own collapsible section. Final=true on the last
// chunk signals the stream is closed.
type CodeExecStdoutMessage struct {
	CallID    string `json:"call_id"`
	TurnID    string `json:"turn_id"`
	Chunk     string `json:"chunk"`
	Final     bool   `json:"final"`
	Truncated bool   `json:"truncated"`
}

// ThinkingMessage carries an intermediate reasoning step to the UI.
type ThinkingMessage struct {
	Content string `json:"content"`
	Phase   string `json:"phase"`
	Source  string `json:"source"`
	TurnID  string `json:"turn_id"`
}

// PlanDisplayMessage carries a plan overview to the UI.
type PlanDisplayMessage struct {
	PlanID  string            `json:"plan_id"`
	Summary string            `json:"summary"`
	Steps   []PlanDisplayStep `json:"steps"`
	Source  string            `json:"source"`
	TurnID  string            `json:"turn_id"`
}

// PlanDisplayStep is a single step for UI display.
type PlanDisplayStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Order       int    `json:"order"`
}

// SessionInfo describes a connected UI session.
type SessionInfo struct {
	ID           string    `json:"id"`
	Transport    string    `json:"transport"`
	ConnectedAt  time.Time `json:"connected_at"`
	UserAgent    string    `json:"user_agent"`
	WorkspaceDir string    `json:"workspace_dir"`
	FilesDir     string    `json:"files_dir"`
}
