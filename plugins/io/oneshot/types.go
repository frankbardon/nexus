package oneshot

import "time"

// oneshotTranscript is the root JSON document emitted by the oneshot plugin.
// It is versioned via the Schema field so downstream consumers can evolve.
type oneshotTranscript struct {
	Schema      string             `json:"schema"`
	SessionID   string             `json:"session_id,omitempty"`
	StartedAt   string             `json:"started_at"`
	EndedAt     string             `json:"ended_at"`
	DurationMS  int64              `json:"duration_ms"`
	FinalOutput string             `json:"final_output"`
	Plans       []planRecord       `json:"plans,omitempty"`
	PlanUpdates []planUpdateRecord `json:"plan_updates,omitempty"`
	Thinking    []thinkingRecord   `json:"thinking,omitempty"`
	Approvals   []approvalRecord   `json:"approvals,omitempty"`
	Errors      []errorRecord      `json:"errors,omitempty"`
}

// planRecord captures a plan.created event (a freshly generated plan).
type planRecord struct {
	PlanID  string           `json:"plan_id"`
	Summary string           `json:"summary,omitempty"`
	Source  string           `json:"source,omitempty"`
	TurnID  string           `json:"turn_id,omitempty"`
	Steps   []planStepRecord `json:"steps"`
}

// planUpdateRecord captures an agent.plan event (a running plan status update).
type planUpdateRecord struct {
	TurnID string           `json:"turn_id,omitempty"`
	Steps  []planStepRecord `json:"steps"`
}

// planStepRecord is one step inside a plan or plan update.
type planStepRecord struct {
	ID          string `json:"id,omitempty"`
	Description string `json:"description"`
	Status      string `json:"status,omitempty"`
	Order       int    `json:"order,omitempty"`
}

// thinkingRecord captures a thinking.step event.
type thinkingRecord struct {
	TurnID    string    `json:"turn_id,omitempty"`
	Source    string    `json:"source,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// approvalRecord captures an auto-approved approval/ask request.
// Kind is "tool", "plan", or "ask".
type approvalRecord struct {
	Kind         string `json:"kind"`
	PromptID     string `json:"prompt_id,omitempty"`
	Description  string `json:"description,omitempty"`
	ToolCall     string `json:"tool_call,omitempty"`
	Risk         string `json:"risk,omitempty"`
	AutoApproved bool   `json:"auto_approved"`
}

// errorRecord captures a core.error event or an error-role io.output message.
type errorRecord struct {
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
	TurnID  string `json:"turn_id,omitempty"`
}
