package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// runLoop drives a stage with a LoopConfig: iterate 1..max_iterations,
// re-dispatching the stage per iteration and grading the iteration against
// loop.until. On convergence, promote the latest iteration artifact to the
// plain stage path and return it. On exhaustion, fall through to
// handleLoopExhaustion.
func (o *Orchestrator) runLoop(ctx context.Context, stage *workspace.Stage) (string, error) {
	res, err := o.runLoopInner(ctx, stage, "")
	if err != nil {
		return "", err
	}
	return res.path, nil
}

// runLoopInner is the shared body used by both top-level loops (plain
// loop stages) and fan-out + loop composition. The itemID + itemValue
// arguments scope artifact paths under items/<itemID>/iter_NN/ when set.
func (o *Orchestrator) runLoopInner(ctx context.Context, stage *workspace.Stage, itemID string) (invocationResult, error) {
	loop := stage.Loop
	if loop == nil {
		return invocationResult{}, fmt.Errorf("runLoop called on stage %q without loop config", stage.ID)
	}

	var (
		prevArt   []byte
		prevIdx   int
		prevFails []icmtypes.ConditionResult
		prevPath  string
	)

	for iter := 1; iter <= loop.MaxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return invocationResult{}, err
		}

		// Emit icm.stage.iteration with the prior iteration's exit
		// failures (empty on iter 1).
		if o.Bus != nil {
			_ = o.Bus.Emit("icm.stage.iteration", icmtypes.ICMStageIteration{
				SchemaVersion: icmtypes.ICMStageIterationVersion,
				RunID:         o.RunID,
				StageID:       stage.ID,
				ItemID:        itemID,
				Iteration:     iter,
				MaxIterations: loop.MaxIterations,
				ExitFailures:  prevFails,
			})
		}

		// Record iteration state.
		o.withState(func(_ *session.RunState) {
			state := o.stageStateRef(stage.ID)
			state.Iterations = appendIteration(state.Iterations, session.IterationState{
				Index:     iter,
				Status:    session.StageStatusRunning,
				StartedAt: time.Now().UTC(),
			})
		})
		_ = o.saveState()

		ic := invocationCtx{
			stage:         stage,
			iteration:     iter,
			prevIterArt:   prevArt,
			prevIterIdx:   prevIdx,
			prevIterFails: prevFails,
			prevIterPath:  prevPath,
			itemID:        itemID,
		}
		result, err := o.runInvocationFull(ctx, ic)
		if err != nil {
			return invocationResult{}, err
		}

		// Evaluate loop.until against the iteration artifact.
		evalCtx := predicates.StageEvalContext{
			RunID:                 o.RunID,
			StageID:               stage.ID,
			ItemID:                itemID,
			Iteration:             iter,
			Container:             "loop.until",
			ParentTurnID:          o.ParentTurnID,
			ParentDepth:           o.ParentDepth,
			WorkspaceRoot:         o.Workflow.Root,
			StageBudgetTimeoutSec: stage.Agent.Budget.TimeoutSeconds,
			InstanceID:            o.InstanceID,
		}
		passed, untilResults := o.Evaluator.EvaluateAll(ctx, loop.Until, result.output, evalCtx)
		untilConds := resultsToConditions(untilResults)

		// Update iteration state with the eval outcome.
		o.withState(func(_ *session.RunState) {
			state := o.stageStateRef(stage.ID)
			idx := len(state.Iterations) - 1
			state.Iterations[idx].CompletedAt = time.Now().UTC()
			state.Iterations[idx].TurnCount = result.turnsUsed
			state.Iterations[idx].ExitResults = untilConds
			if passed {
				state.Iterations[idx].Status = session.StageStatusDone
			} else {
				state.Iterations[idx].Status = session.StageStatusFailed
			}
		})
		_ = o.saveState()

		if passed {
			// Convergence — promote the iteration artifact to the plain
			// stage path so downstream references resolve naturally.
			plainPath, perr := o.promoteIterationArtifact(stage, itemID, iter, result.output, result.validators, result.delegateOut, false)
			if perr != nil {
				return invocationResult{}, perr
			}
			result.path = plainPath
			return result, nil
		}

		// Track failures for next iteration's PreviousIteration block.
		prevArt = result.output
		prevIdx = iter
		prevFails = failingConditions(untilConds)
		prevPath = stage.ID + "/" + stage.Output.Filename
	}

	// Budget exhausted: write a convergence-failed marker on the last
	// iteration artifact and route through handleLoopExhaustion.
	finalIter := loop.MaxIterations
	finalIterPath := iterationArtifactPath(o.Session, stage.ID, itemID, stage.Output.Filename, finalIter)
	if err := o.writeConvergenceFailed(finalIterPath, stage, finalIter); err != nil {
		return invocationResult{}, err
	}

	return o.handleLoopExhaustion(ctx, stage, itemID, prevArt, finalIter)
}

// handleLoopExhaustion routes through the loop.on_exhausted policy:
//   - error: return a failure.
//   - human_gate (default): ask the operator; allow → promote final iter,
//     reject → fail, restart → recursively re-enter runLoopInner subject
//     to LoopMaxRestarts.
func (o *Orchestrator) handleLoopExhaustion(ctx context.Context, stage *workspace.Stage, itemID string, finalContent []byte, finalIter int) (invocationResult, error) {
	loop := stage.Loop
	action := loop.OnExhausted
	if action == "" {
		action = workspace.ExhaustedHumanGate
	}

	switch action {
	case workspace.ExhaustedError:
		return invocationResult{}, fmt.Errorf("stage %s loop exhausted after %d iterations", stage.ID, finalIter)

	case workspace.ExhaustedHumanGate:
		// Fan-out + loop composition has no per-item HITL — per the locked
		// design, item-scoped exhaustion is automatic failure.
		if itemID != "" {
			return invocationResult{}, fmt.Errorf("item %s loop exhausted after %d iterations", itemID, finalIter)
		}
		if o.HITLDispatch == nil {
			return invocationResult{}, fmt.Errorf("stage %s loop exhausted (no HITL plumbed)", stage.ID)
		}
		var atCap bool
		o.withState(func(_ *session.RunState) {
			state := o.stageStateRef(stage.ID)
			if o.LoopMaxRestarts > 0 && state.RestartCount >= o.LoopMaxRestarts {
				atCap = true
			}
		})
		if atCap {
			return invocationResult{}, fmt.Errorf("stage %s exceeded loop_max_restarts=%d", stage.ID, o.LoopMaxRestarts)
		}
		reqID := o.newHITLID("icm.loop.exhausted", stage.ID, fmt.Sprintf("iter%d", finalIter))
		req := events.HITLRequest{
			SchemaVersion:   events.HITLRequestVersion,
			ID:              reqID,
			TurnID:          o.ParentTurnID,
			RequesterPlugin: o.InstanceID,
			ActionKind:      "icm.loop.exhausted",
			ActionRef: map[string]any{
				"run_id":         o.RunID,
				"stage_id":       stage.ID,
				"final_iter":     finalIter,
				"max_iterations": loop.MaxIterations,
			},
			Mode: events.HITLModeBoth,
			Choices: []events.HITLChoice{
				{ID: "allow", Label: "Accept handoff", Kind: events.ChoiceAllow},
				{ID: "restart", Label: "Restart loop", Kind: events.ChoiceCustom},
				{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
			},
			Prompt: fmt.Sprintf("Loop on stage %s did not converge after %d iterations.", stageLabel(stage), finalIter),
			Metadata: map[string]any{
				"icm.kind":   "loop_exhausted",
				"icm.run_id": o.RunID,
				"icm.stage":  stage.ID,
			},
		}
		resp, err := o.HITLDispatch(ctx, req)
		if err != nil {
			return invocationResult{}, fmt.Errorf("loop exhausted gate: %w", err)
		}
		if resp.Cancelled {
			return invocationResult{}, fmt.Errorf("loop exhausted gate cancelled: %s", resp.CancelReason)
		}
		switch resp.ChoiceID {
		case "allow":
			path, perr := o.promoteIterationArtifact(stage, itemID, finalIter, finalContent, nil, defaultDelegateOutput(), true)
			if perr != nil {
				return invocationResult{}, perr
			}
			return invocationResult{
				path:              path,
				output:            finalContent,
				convergenceFailed: true,
				turnsUsed:         finalIter,
			}, nil
		case "restart":
			// Wipe the stage subtree and re-enter the loop. We track
			// RestartCount on state so the LoopMaxRestarts cap holds.
			if err := o.Session.ClearStage(stage.ID); err != nil {
				return invocationResult{}, fmt.Errorf("clear stage on loop restart: %w", err)
			}
			o.withState(func(_ *session.RunState) {
				state := o.stageStateRef(stage.ID)
				state.RestartCount++
				state.Iterations = nil
				state.Status = session.StageStatusRunning
			})
			_ = o.saveState()
			return o.runLoopInner(ctx, stage, itemID)
		case "reject":
			return invocationResult{}, fmt.Errorf("loop exhausted rejected: %w", errRejected)
		default:
			return invocationResult{}, fmt.Errorf("loop exhausted gate: unknown choice %q", resp.ChoiceID)
		}

	default:
		return invocationResult{}, fmt.Errorf("unknown loop.on_exhausted %q", action)
	}
}

// promoteIterationArtifact copies the final iteration's artifact to the
// plain stage path (or, for fan-out, the per-item path) so downstream
// references can resolve cleanly. The sidecar reflects the final
// iteration count.
func (o *Orchestrator) promoteIterationArtifact(stage *workspace.Stage, itemID string, finalIter int, content []byte, validators []icmtypes.ConditionResult, _ any, convergenceFailed bool) (string, error) {
	var path string
	if itemID != "" {
		path = o.Session.ItemArtifactPath(stage.ID, itemID, stage.Output.Filename)
	} else {
		path = o.Session.ArtifactPath(stage.ID, stage.Output.Filename)
	}
	if err := o.Session.WriteArtifact(path, content); err != nil {
		return "", err
	}
	meta := session.ArtifactMeta{
		StageID:           stage.ID,
		IterationsRun:     finalIter,
		ConvergenceFailed: convergenceFailed,
		ValidatorsPassed:  passingConditions(validators),
		UnmetConditions:   failingConditions(validators),
		WrittenAt:         time.Now().UTC(),
	}
	if err := o.Session.WriteSidecar(path, meta); err != nil {
		return path, err
	}
	return path, nil
}

// writeConvergenceFailed re-writes the final iteration's sidecar with the
// convergence-failed marker set. The artifact body is left untouched —
// only the sidecar surfaces the marker so reviewers see it.
func (o *Orchestrator) writeConvergenceFailed(artifactPath string, stage *workspace.Stage, iter int) error {
	meta := session.ArtifactMeta{
		StageID:           stage.ID,
		IterationsRun:     iter,
		ConvergenceFailed: true,
		WrittenAt:         time.Now().UTC(),
	}
	return o.Session.WriteSidecar(artifactPath, meta)
}

// iterationArtifactPath returns the artifact path for a given iteration
// scoped by itemID (or not).
func iterationArtifactPath(s *session.Session, stageID, itemID, filename string, iter int) string {
	if itemID != "" {
		return s.ItemIterationArtifactPath(stageID, itemID, filename, iter)
	}
	return s.IterationArtifactPath(stageID, filename, iter)
}

// appendIteration appends an iteration entry, replacing an existing entry
// with the same index when present (defensive — runLoopInner only inserts
// once per iteration, but a restart path might re-enter mid-stream).
func appendIteration(items []session.IterationState, it session.IterationState) []session.IterationState {
	for i := range items {
		if items[i].Index == it.Index {
			items[i] = it
			return items
		}
	}
	return append(items, it)
}

// defaultDelegateOutput returns a zero-value Output marker for callers
// that need to populate a sidecar's Delegate block without a real
// dispatch (e.g. the loop-exhaustion handoff path).
func defaultDelegateOutput() any { return struct{}{} }
