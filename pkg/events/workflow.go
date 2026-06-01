package events

// Schema-version constants for workflow.* payloads. See doc.go.
const (
	WorkflowProgressVersion = 1
)

// Workflow* status constants are the closed set Status field values for
// WorkflowProgress. Workflow plugins (e.g. nexus.workflows.icm,
// nexus.agent.planexec) emit one event per state transition.
const (
	WorkflowStatusStarted   = "started"
	WorkflowStatusRunning   = "running"
	WorkflowStatusIterating = "iterating"
	WorkflowStatusItemDone  = "item_done"
	WorkflowStatusCompleted = "completed"
	WorkflowStatusFailed    = "failed"
	WorkflowStatusHalted    = "halted"
)

// WorkflowProgress is a workflow-agnostic progress payload that any
// multi-stage workflow plugin can emit. Subscribers — typically IO
// plugins — render a dedicated workflow status surface (a sticky panel
// in the TUI right rail, an indicator chip in the browser) from a
// single canonical event class, regardless of which plugin produced
// the event.
//
// ICM emits WorkflowProgress *alongside* its detailed icm.* events:
// the icm.* stream feeds the scrollback audit trail, WorkflowProgress
// feeds the dedicated panel. Future workflow plugins (planexec,
// orchestrator) can emit just this event and inherit the same UI
// treatment without a per-plugin event subscription.
type WorkflowProgress struct {
	SchemaVersion int `json:"_schema_version"`

	// WorkflowID identifies the producer (plugin instance ID, e.g.
	// "nexus.workflows.icm" or "nexus.workflows.icm/script").
	WorkflowID string `json:"workflow_id"`
	// WorkflowName is a human-readable label for the workflow as a
	// whole (the workspace name for ICM, the plan summary for planexec).
	WorkflowName string `json:"workflow_name,omitempty"`
	// RunID identifies this particular run within the workflow.
	RunID string `json:"run_id"`

	// Stage is the machine ID of the current stage (e.g. "04_assemble").
	// Empty when Status is Started before any stage dispatches, or
	// Completed/Halted at run end.
	Stage string `json:"stage,omitempty"`
	// StageLabel is the human-readable display string for the stage.
	StageLabel string `json:"stage_label,omitempty"`
	// StageIndex is the 1-based position of the current stage.
	StageIndex int `json:"stage_index,omitempty"`
	// StageTotal is the total number of stages in this workflow.
	StageTotal int `json:"stage_total,omitempty"`

	// Iteration is the current loop iteration within Stage. 0 when the
	// stage is not iterating.
	Iteration int `json:"iteration,omitempty"`
	// MaxIterations is the loop cap for the current stage. 0 when
	// non-looping.
	MaxIterations int `json:"max_iterations,omitempty"`

	// Turn is the current inner turn within the current stage
	// invocation. 0 when not tracked.
	Turn int `json:"turn,omitempty"`
	// MaxTurns is the inner-turn cap. 0 when non-bounded.
	MaxTurns int `json:"max_turns,omitempty"`

	// ItemsDone / ItemsTotal report fan-out progress; both 0 when the
	// stage is not a fan-out.
	ItemsDone  int `json:"items_done,omitempty"`
	ItemsTotal int `json:"items_total,omitempty"`
	// CurrentItem is the ID of the most recently completed item, when
	// fan-out applies.
	CurrentItem string `json:"current_item,omitempty"`

	// Status is the lifecycle marker. One of WorkflowStatus* constants
	// above. Required.
	Status string `json:"status"`
	// Detail is a short free-form one-liner suitable for display.
	Detail string `json:"detail,omitempty"`
	// Failures lists predicate names that caused the most recent
	// non-converging iteration or turn. Empty when not applicable.
	Failures []string `json:"failures,omitempty"`
}
