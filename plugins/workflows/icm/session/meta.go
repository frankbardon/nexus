package session

import (
	"time"

	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
)

// RunMeta is the immutable per-run metadata written exactly once at
// session creation to <runID>/.icm/run.json. Used by operators and
// downstream tooling to inspect a finished or in-flight run.
type RunMeta struct {
	RunID          string         `json:"run_id"`
	InstanceID     string         `json:"instance_id"`
	WorkspaceRoot  string         `json:"workspace_root"`
	WorkspaceName  string         `json:"workspace_name"`
	StartedAt      time.Time      `json:"started_at"`
	ConfigSnapshot map[string]any `json:"config_snapshot,omitempty"`
}

// DelegateMeta captures the budget + posture identity of the sub-agent
// invocation that produced an artifact. Embedded inside ArtifactMeta.
type DelegateMeta struct {
	PostureName    string `json:"posture_name,omitempty"`
	PostureVersion string `json:"posture_version,omitempty"`
	TokensUsed     int    `json:"tokens_used,omitempty"`
	ToolCallsUsed  int    `json:"tool_calls_used,omitempty"`
	ElapsedMS      int64  `json:"elapsed_ms,omitempty"`
}

// ArtifactMeta is the per-artifact sidecar payload written to
// <artifact>.icm.json beside every artifact. The orchestrator populates
// it from the run state + the invocation that produced the artifact.
type ArtifactMeta struct {
	StageID           string                     `json:"stage_id"`
	IterationsRun     int                        `json:"iterations_run,omitempty"`
	ConvergenceFailed bool                       `json:"convergence_failed,omitempty"`
	UnmetConditions   []icmtypes.ConditionResult `json:"unmet_conditions,omitempty"`
	ValidatorsPassed  []icmtypes.ConditionResult `json:"validators_passed,omitempty"`
	GroundingHashes   map[string]string          `json:"grounding_hashes,omitempty"`
	ParentArtifacts   []string                   `json:"parent_artifacts,omitempty"`
	Delegate          DelegateMeta               `json:"delegate,omitempty"`
	WrittenAt         time.Time                  `json:"written_at"`
}

// ConditionResult is re-exported from icmtypes for callers that already
// import the session package. Sidecars + state.json reference these in
// their JSON.
type ConditionResult = icmtypes.ConditionResult
