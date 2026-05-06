package events

// Schema-version constants for plan.* payloads. See doc.go.
const (
	PlanRequestVersion  = 1
	PlanResultVersion   = 1
	PlanProgressVersion = 1
)

// PlanRequest asks an active planner to generate a plan for the given input.
type PlanRequest struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID    string
	SessionID string
	Input     string
}

// PlanResult carries a completed plan back to the agent for execution.
type PlanResult struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID   string
	PlanID   string
	Steps    []PlanResultStep
	Summary  string
	Approved bool
	Source   string // "dynamic" or "static"
}

// PlanResultStep is a single step in a planner-generated plan.
type PlanResultStep struct {
	ID           string
	Description  string // user-facing display text
	Instructions string // detailed instructions for the agent; defaults to Description if empty
	Status       string // "pending", "active", "completed", "failed"
	Order        int
}

// PlanProgress reports a step status change during plan execution.
type PlanProgress struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID string
	PlanID string
	StepID string
	Status string // "pending", "active", "completed", "failed"
	Detail string
}
