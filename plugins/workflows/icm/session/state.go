package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
)

// StageStatus is the lifecycle state of a single stage in a RunState.
type StageStatus string

// Stage lifecycle values written into state.json.
const (
	StageStatusPending     StageStatus = "pending"
	StageStatusRunning     StageStatus = "running"
	StageStatusGatePending StageStatus = "gate_pending"
	StageStatusDone        StageStatus = "done"
	StageStatusFailed      StageStatus = "failed"
)

// Run outcome values written into RunState.Outcome.
const (
	OutcomeRunning   = "running"
	OutcomeCompleted = "completed"
	OutcomeHalted    = "halted"
	OutcomeRejected  = "rejected"
	OutcomeCancelled = "cancelled"
)

// RunState is the orchestrator's view of a run's progress. Persisted to
// <session>/.icm/state.json after every state transition. Enables
// inspection today and (in a future version) resume.
type RunState struct {
	RunID               string                   `json:"run_id"`
	InstanceID          string                   `json:"instance_id,omitempty"`
	WorkspaceRoot       string                   `json:"workspace_root,omitempty"`
	WorkspaceDocSummary string                   `json:"workspace_doc_summary,omitempty"`
	StartedAt           time.Time                `json:"started_at"`
	UpdatedAt           time.Time                `json:"updated_at"`
	CurrentStage        int                      `json:"current_stage"`
	Outcome             string                   `json:"outcome,omitempty"`
	Stages              []StageState             `json:"stages,omitempty"`
	Verifiers           map[string]VerifierState `json:"verifiers,omitempty"`
}

// StageState tracks the progress of a single stage.
type StageState struct {
	ID           string           `json:"id"`
	Status       StageStatus      `json:"status"`
	StartedAt    time.Time        `json:"started_at,omitempty"`
	CompletedAt  time.Time        `json:"completed_at,omitempty"`
	TurnCount    int              `json:"turn_count,omitempty"`
	RestartCount int              `json:"restart_count,omitempty"`
	Iterations   []IterationState `json:"iterations,omitempty"`
	Items        []ItemState      `json:"items,omitempty"`
}

// IterationState tracks a single iteration of a looping stage.
type IterationState struct {
	Index       int                        `json:"index"`
	Status      StageStatus                `json:"status"`
	StartedAt   time.Time                  `json:"started_at,omitempty"`
	CompletedAt time.Time                  `json:"completed_at,omitempty"`
	TurnCount   int                        `json:"turn_count,omitempty"`
	ExitResults []icmtypes.ConditionResult `json:"exit_results,omitempty"`
}

// ItemState tracks a single item of a fan-out stage. Iterations is
// populated when the stage has both loop and fan_out modes composed.
type ItemState struct {
	ID          string           `json:"id"`
	Index       int              `json:"index"`
	Status      StageStatus      `json:"status"`
	StartedAt   time.Time        `json:"started_at,omitempty"`
	CompletedAt time.Time        `json:"completed_at,omitempty"`
	Error       string           `json:"error,omitempty"`
	Iterations  []IterationState `json:"iterations,omitempty"`
	Path        string           `json:"path,omitempty"`
}

// VerifierState tracks a single verifier outcome for the run.
type VerifierState struct {
	Status      StageStatus `json:"status"`
	StartedAt   time.Time   `json:"started_at,omitempty"`
	CompletedAt time.Time   `json:"completed_at,omitempty"`
	Verdict     string      `json:"verdict,omitempty"`
	Feedback    string      `json:"feedback,omitempty"`
}

// statePath returns the canonical state.json path under this session.
func (s *Session) statePath() string {
	return filepath.Join(s.RootDir, ".icm", "state.json")
}

// LoadState reads state.json if present. Returns a zero-value RunState
// seeded with the session's RunID and StartedAt (no error) when the
// file does not exist yet.
func (s *Session) LoadState() (*RunState, error) {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &RunState{
				RunID:     s.RunID,
				StartedAt: s.StartedAt,
			}, nil
		}
		return nil, fmt.Errorf("session: read state: %w", err)
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("session: state.json invalid: %w", err)
	}
	return &st, nil
}

// SaveState writes the RunState atomically under stateMu so fan-out
// goroutines that each transition a different stage/item still produce
// a coherent on-disk document. UpdatedAt is stamped here.
func (s *Session) SaveState(st *RunState) error {
	if st == nil {
		return fmt.Errorf("session: SaveState requires non-nil state")
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal state: %w", err)
	}
	return s.WriteArtifact(s.statePath(), data)
}
