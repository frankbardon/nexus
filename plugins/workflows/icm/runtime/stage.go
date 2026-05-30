package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// runStage executes a single stage: optional start gate → shape dispatch
// (plain / loop / fanout) → verifiers → optional end gate. The "restart"
// choice on the end gate re-enters the stage; everything else either
// returns nil (success) or propagates an error (halt).
func (o *Orchestrator) runStage(ctx context.Context, stage *workspace.Stage, order int) error {
	for {
		state := o.stageStateRef(stage.ID)
		state.Status = session.StageStatusRunning
		state.StartedAt = time.Now().UTC()
		state.CompletedAt = time.Time{}
		_ = o.saveState()

		if o.Bus != nil {
			_ = o.Bus.Emit("icm.stage.started", makeStageStarted(o.RunID, stage.ID, o.PostureBuilder.PostureName(stage.ID), order))
		}

		gate := o.humanGateFor(stage)
		if gate == workspace.HumanGateStart || gate == workspace.HumanGateBoth {
			if err := o.runStageGate(ctx, stage, "icm.stage.start"); err != nil {
				return o.failStage(stage, err)
			}
		}

		// Dispatch by shape.
		artifactPath, err := o.dispatchStage(ctx, stage)
		if err != nil {
			return o.failStage(stage, err)
		}

		// Run declared verifiers against the produced artifact path.
		if len(stage.Verifiers) > 0 {
			if err := o.runVerifiers(ctx, stage, artifactPath); err != nil {
				return o.failStage(stage, err)
			}
		}

		// End gate: allow returns, restart loops, reject errors.
		if gate == workspace.HumanGateEnd || gate == workspace.HumanGateBoth {
			restart, err := o.runStageEndGate(ctx, stage)
			if err != nil {
				return o.failStage(stage, err)
			}
			if restart {
				if err := o.Session.ClearStage(stage.ID); err != nil {
					return o.failStage(stage, fmt.Errorf("clear stage on restart: %w", err))
				}
				state := o.stageStateRef(stage.ID)
				state.RestartCount++
				state.Status = session.StageStatusPending
				state.StartedAt = time.Time{}
				state.CompletedAt = time.Time{}
				state.Iterations = nil
				state.Items = nil
				state.TurnCount = 0
				_ = o.saveState()
				continue
			}
		}

		// Stage completed cleanly.
		state = o.stageStateRef(stage.ID)
		state.Status = session.StageStatusDone
		state.CompletedAt = time.Now().UTC()
		_ = o.saveState()

		if o.Bus != nil {
			completed := icmtypes.ICMStageCompleted{
				SchemaVersion: icmtypes.ICMStageCompletedVersion,
				RunID:         o.RunID,
				StageID:       stage.ID,
				ArtifactPath:  artifactPath,
			}
			if stage.Loop != nil {
				completed.IterationsRun = len(state.Iterations)
			}
			_ = o.Bus.Emit("icm.stage.completed", completed)
		}
		return nil
	}
}

// dispatchStage selects the right execution shape for a stage. Returns the
// artifact path that ends up at the canonical stage path (used for verifier
// input + ICMStageCompleted payload).
func (o *Orchestrator) dispatchStage(ctx context.Context, stage *workspace.Stage) (string, error) {
	switch {
	case stage.FanOut != nil:
		return o.runFanOut(ctx, stage)
	case stage.Loop != nil:
		return o.runLoop(ctx, stage)
	default:
		return o.runInvocation(ctx, invocationCtx{stage: stage})
	}
}

// runVerifiers runs each declared verifier against the freshly written
// artifact path. Verifiers are stages themselves (looked up in
// workflow.Verifiers) — invoke them through the same runInvocation surface
// so their own validators, payloads, and budgets apply.
func (o *Orchestrator) runVerifiers(ctx context.Context, stage *workspace.Stage, artifactPath string) error {
	for _, id := range stage.Verifiers {
		v := o.Workflow.Verifiers[id]
		if v == nil {
			return fmt.Errorf("verifier %q not registered in workflow", id)
		}
		if o.state.Verifiers == nil {
			o.state.Verifiers = make(map[string]session.VerifierState)
		}
		vs := session.VerifierState{
			Status:    session.StageStatusRunning,
			StartedAt: time.Now().UTC(),
		}
		o.state.Verifiers[id] = vs
		_ = o.saveState()

		// Verifiers read their own inputs (via Inputs.Artifacts); we do not
		// pass the upstream artifact path explicitly.
		_ = artifactPath
		path, err := o.runInvocation(ctx, invocationCtx{stage: v})
		updated := o.state.Verifiers[id]
		updated.CompletedAt = time.Now().UTC()
		if err != nil {
			updated.Status = session.StageStatusFailed
			updated.Verdict = "error"
			updated.Feedback = err.Error()
			o.state.Verifiers[id] = updated
			_ = o.saveState()
			return fmt.Errorf("verifier %q: %w", id, err)
		}
		updated.Status = session.StageStatusDone
		updated.Verdict = "pass"
		o.state.Verifiers[id] = updated
		_ = o.saveState()
		_ = path
	}
	return nil
}

// runStageGate emits an icm.stage.start gate and returns nil only on
// "allow". Reject + cancellation surface as errors.
func (o *Orchestrator) runStageGate(ctx context.Context, stage *workspace.Stage, kind string) error {
	if o.HITLDispatch == nil {
		return nil // no HITL plumbed — auto-allow.
	}

	state := o.stageStateRef(stage.ID)
	state.Status = session.StageStatusGatePending
	_ = o.saveState()

	reqID := o.newHITLID(kind, stage.ID, "")
	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              reqID,
		TurnID:          o.ParentTurnID,
		RequesterPlugin: o.InstanceID,
		ActionKind:      kind,
		ActionRef: map[string]any{
			"run_id":   o.RunID,
			"stage_id": stage.ID,
		},
		Mode: events.HITLModeBoth,
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow},
			{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
		},
		Prompt: fmt.Sprintf("Approve start of stage %s?", stageLabel(stage)),
		Metadata: map[string]any{
			"icm.kind":     "stage_gate",
			"icm.position": "start",
			"icm.run_id":   o.RunID,
			"icm.stage":    stage.ID,
		},
	}
	resp, err := o.HITLDispatch(ctx, req)
	if err != nil {
		return fmt.Errorf("stage start gate: %w", err)
	}
	if resp.Cancelled {
		return fmt.Errorf("stage start gate cancelled: %s", resp.CancelReason)
	}
	switch resp.ChoiceID {
	case "allow":
		return nil
	case "reject":
		return fmt.Errorf("stage %s rejected at start gate: %w", stage.ID, errRejected)
	default:
		return fmt.Errorf("stage start gate: unknown choice %q", resp.ChoiceID)
	}
}

// runStageEndGate emits an icm.stage.end gate. Returns restart=true to
// signal the caller should clear + re-enter the stage; otherwise nil
// indicates "allow", an error indicates "reject" or cancellation.
func (o *Orchestrator) runStageEndGate(ctx context.Context, stage *workspace.Stage) (bool, error) {
	if o.HITLDispatch == nil {
		return false, nil
	}

	state := o.stageStateRef(stage.ID)
	state.Status = session.StageStatusGatePending
	_ = o.saveState()

	reqID := o.newHITLID("icm.stage.end", stage.ID, "")
	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              reqID,
		TurnID:          o.ParentTurnID,
		RequesterPlugin: o.InstanceID,
		ActionKind:      "icm.stage.end",
		ActionRef: map[string]any{
			"run_id":   o.RunID,
			"stage_id": stage.ID,
		},
		Mode: events.HITLModeBoth,
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow},
			{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
			{ID: "restart", Label: "Restart", Kind: events.ChoiceCustom},
		},
		Prompt: fmt.Sprintf("Approve completion of stage %s?", stageLabel(stage)),
		Metadata: map[string]any{
			"icm.kind":     "stage_gate",
			"icm.position": "end",
			"icm.run_id":   o.RunID,
			"icm.stage":    stage.ID,
		},
	}
	resp, err := o.HITLDispatch(ctx, req)
	if err != nil {
		return false, fmt.Errorf("stage end gate: %w", err)
	}
	if resp.Cancelled {
		return false, fmt.Errorf("stage end gate cancelled: %s", resp.CancelReason)
	}
	switch resp.ChoiceID {
	case "allow":
		return false, nil
	case "restart":
		return true, nil
	case "reject":
		return false, fmt.Errorf("stage %s rejected at end gate: %w", stage.ID, errRejected)
	default:
		return false, fmt.Errorf("stage end gate: unknown choice %q", resp.ChoiceID)
	}
}

// failStage updates the stage RunState to failed and emits ICMStageFailed.
// The caller's error is returned unchanged so wrapping survives.
func (o *Orchestrator) failStage(stage *workspace.Stage, cause error) error {
	if o.state != nil {
		state := o.stageStateRef(stage.ID)
		state.Status = session.StageStatusFailed
		state.CompletedAt = time.Now().UTC()
		_ = o.saveState()
	}
	if o.Bus != nil {
		_ = o.Bus.Emit("icm.stage.failed", icmtypes.ICMStageFailed{
			SchemaVersion: icmtypes.ICMStageFailedVersion,
			RunID:         o.RunID,
			StageID:       stage.ID,
			Reason:        cause.Error(),
		})
	}
	return cause
}

// newHITLID delegates to the plugin-supplied generator, falling back to a
// deterministic local prefix when tests construct an orchestrator without
// wiring NewHITLID. The prefix is always "icm-" so HITL plugin filtering
// downstream stays uniform.
func (o *Orchestrator) newHITLID(kind, stageID, extra string) string {
	if o.NewHITLID != nil {
		return o.NewHITLID(kind, o.RunID, stageID, extra)
	}
	// Fallback: still emit a stable-looking ID so even nil-NewHITLID test
	// flows do not collide. We do NOT include randomness here because
	// production paths must use NewHITLID.
	return "icm-" + kind + "-" + o.RunID + "-" + stageID + "-" + extra
}

// stageLabel returns the stage's Display label or, when empty, its ID.
func stageLabel(s *workspace.Stage) string {
	if s.Display != "" {
		return s.Display
	}
	return s.ID
}

// makeStageStarted assembles the ICMStageStarted event payload.
func makeStageStarted(runID, stageID, posture string, order int) icmtypes.ICMStageStarted {
	return icmtypes.ICMStageStarted{
		SchemaVersion: icmtypes.ICMStageStartedVersion,
		RunID:         runID,
		StageID:       stageID,
		PostureName:   posture,
		Order:         order,
	}
}

