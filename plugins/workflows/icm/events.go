package icm

import "github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"

// Schema-version constants for icm.* event payloads. ICM emits these on
// top of the generic plan.created/plan.progress surface so basic UIs see
// stage-level transitions while richer UIs render iteration/turn/item
// detail.

const (
	ICMRunStartedVersion       = 1
	ICMRunCompletedVersion     = 1
	ICMRunHaltedVersion        = 1
	ICMStageStartedVersion     = 1
	ICMStageCompletedVersion   = 1
	ICMStageFailedVersion      = 1
	ICMStageIterationVersion   = 1
	ICMTurnVersion             = 1
	ICMFanoutItemVersion       = 1
	ICMPredicateFailedVersion  = 1
)

// ConditionResult is re-exported from icmtypes so existing
// icm.ConditionResult references in this package and its sub-packages
// keep working. The canonical definition lives in icmtypes to avoid a
// cycle with the session sub-package.
type ConditionResult = icmtypes.ConditionResult

// ICMRunStarted is emitted after workspace load + plan.created, before
// the first stage dispatches.
type ICMRunStarted struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	InstanceID    string `json:"instance_id"`
	WorkspaceRoot string `json:"workspace_root"`
	WorkspaceName string `json:"workspace_name"`
	Stages        int    `json:"stages"`
}

// ICMRunCompleted fires once all stages finish without halt.
type ICMRunCompleted struct {
	SchemaVersion  int    `json:"_schema_version"`
	RunID          string `json:"run_id"`
	StagesRun      int    `json:"stages_run"`
	AggregatePath  string `json:"aggregate_path,omitempty"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

// ICMRunHalted fires when a stage error policy halts the run, a human
// gate rejects, or the run context is cancelled.
type ICMRunHalted struct {
	SchemaVersion  int    `json:"_schema_version"`
	RunID          string `json:"run_id"`
	Reason         string `json:"reason"`
	HaltedAtStage  string `json:"halted_at_stage,omitempty"`
	Cancelled      bool   `json:"cancelled,omitempty"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

// ICMStageStarted fires when stage execution begins (before any
// human_gate: start gate).
type ICMStageStarted struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	PostureName   string `json:"posture_name"`
	Order         int    `json:"order"`
}

// ICMStageCompleted fires after artifact is written and any end gate
// resolves.
type ICMStageCompleted struct {
	SchemaVersion     int    `json:"_schema_version"`
	RunID             string `json:"run_id"`
	StageID           string `json:"stage_id"`
	ArtifactPath      string `json:"artifact_path,omitempty"`
	IterationsRun     int    `json:"iterations_run,omitempty"`
	ConvergenceFailed bool   `json:"convergence_failed,omitempty"`
}

// ICMStageFailed fires when a stage halts due to dispatch error policy,
// rejected gate, or predicate exhaustion under loop on_exhausted: error.
type ICMStageFailed struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	Reason        string `json:"reason"`
}

// ICMStageIteration fires once per loop iteration, immediately before
// the iteration's runInvocation. ItemID populated for fan-out + loop
// composition.
type ICMStageIteration struct {
	SchemaVersion int               `json:"_schema_version"`
	RunID         string            `json:"run_id"`
	StageID       string            `json:"stage_id"`
	ItemID        string            `json:"item_id,omitempty"`
	Iteration     int               `json:"iteration"`
	MaxIterations int               `json:"max_iterations"`
	ExitFailures  []ConditionResult `json:"exit_failures,omitempty"`
}

// ICMTurn fires after each turn within an invocation (richer-UIs only;
// stage-level progress already surfaces via plan.progress).
type ICMTurn struct {
	SchemaVersion int               `json:"_schema_version"`
	RunID         string            `json:"run_id"`
	StageID       string            `json:"stage_id"`
	ItemID        string            `json:"item_id,omitempty"`
	Iteration     int               `json:"iteration,omitempty"`
	Turn          int               `json:"turn"`
	MaxTurns      int               `json:"max_turns"`
	LastFailures  []ConditionResult `json:"last_failures,omitempty"`
}

// ICMFanoutItem fires at each item lifecycle boundary in a fan-out
// stage (active → completed | failed).
type ICMFanoutItem struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	ItemID        string `json:"item_id"`
	Index         int    `json:"index"`
	Total         int    `json:"total"`
	Status        string `json:"status"` // active | completed | failed
	Error         string `json:"error,omitempty"`
}

// ICMPredicateFailed fires whenever any predicate evaluation returns
// Verdict=false. Single source of truth for failure visibility — pass
// paths are not emitted.
type ICMPredicateFailed struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	ItemID        string `json:"item_id,omitempty"`
	Container     string `json:"container"` // output.validators | loop.until | verifier
	PredicateName string `json:"predicate_name"`
	PredicateType string `json:"predicate_type"`
	Feedback      string `json:"feedback,omitempty"`
}
