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
//
// SpawnID, when set, links the step to a subagent worker so the UI can
// inline progress (iteration count, tool calls, terminal totals) onto
// the plan step. Empty when the agent executes the step inline.
type PlanDisplayStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Order       int    `json:"order"`
	SpawnID     string `json:"spawn_id,omitempty"`
}

// WorkerStatusMessage carries a single subagent lifecycle event to the UI.
//
// One worker_status envelope is sent per bus event in the
// subagent.started → subagent.iteration* → subagent.complete sequence,
// keyed by SpawnID. The frontend tracks workers by SpawnID and updates
// in place on each new envelope. Kind is the discriminator:
//
//	"started"   — worker just began; Task is set, Iteration=0.
//	"iteration" — worker finished iteration N; Content/ToolCount describe
//	              the assistant turn that just landed.
//	"complete"  — worker finished (success or error). Result/Error/
//	              Iterations/TotalTokens summarize the run.
//
// ParentTurnID lets the UI correlate workers with the orchestrator turn
// that spawned them, so progress can clear at the start of the next turn.
type WorkerStatusMessage struct {
	Kind         string `json:"kind"`
	SpawnID      string `json:"spawn_id"`
	Task         string `json:"task,omitempty"`
	Iteration    int    `json:"iteration,omitempty"`
	Content      string `json:"content,omitempty"`
	ToolCount    int    `json:"tool_count,omitempty"`
	Result       string `json:"result,omitempty"`
	Error        string `json:"error,omitempty"`
	Iterations   int    `json:"iterations,omitempty"`
	TotalTokens  int    `json:"total_tokens,omitempty"`
	ParentTurnID string `json:"parent_turn_id,omitempty"`
}

// WorkflowStatusMessage powers a dedicated workflow status surface in the
// UI — a sticky panel in the TUI right rail; a header indicator in the
// browser. One message per workflow.progress bus event.
//
// IO plugins translate events.WorkflowProgress into this shape so the
// UI rendering code is provider-agnostic: ICM, planexec, and any future
// workflow plugin look the same in the panel.
type WorkflowStatusMessage struct {
	WorkflowID    string   `json:"workflow_id"`
	WorkflowName  string   `json:"workflow_name,omitempty"`
	RunID         string   `json:"run_id"`
	Stage         string   `json:"stage,omitempty"`
	StageLabel    string   `json:"stage_label,omitempty"`
	StageIndex    int      `json:"stage_index,omitempty"`
	StageTotal    int      `json:"stage_total,omitempty"`
	Iteration     int      `json:"iteration,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	Turn          int      `json:"turn,omitempty"`
	MaxTurns      int      `json:"max_turns,omitempty"`
	ItemsDone     int      `json:"items_done,omitempty"`
	ItemsTotal    int      `json:"items_total,omitempty"`
	CurrentItem   string   `json:"current_item,omitempty"`
	Status        string   `json:"status"`
	Detail        string   `json:"detail,omitempty"`
	Failures      []string `json:"failures,omitempty"`
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
