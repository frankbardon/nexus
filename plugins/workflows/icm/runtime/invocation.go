package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// invocationCtx is the per-invocation context passed into runInvocation.
// It collects the in-progress stage state (iteration / item / iteration-
// path) the turn loop needs to render the right payload and persist the
// artifact in the right location.
type invocationCtx struct {
	// stage is the Stage being dispatched. Verifier invocations pass the
	// verifier here directly.
	stage *workspace.Stage

	// Loop bookkeeping (zero for non-loop invocations).
	iteration int
	// prevIterArt is the prior iteration's artifact bytes, used as the
	// PreviousIteration body in the payload.
	prevIterArt []byte
	// prevIterIdx is the prior iteration's 1-based index.
	prevIterIdx int
	// prevIterFails carries the loop exit-failures that triggered the next
	// iteration.
	prevIterFails []icmtypes.ConditionResult
	// prevIterPath is the prior iteration's artifact logical ref
	// ("<stage>/<filename>").
	prevIterPath string

	// Fan-out bookkeeping (zero for non-fanout invocations).
	itemID    string
	itemValue any
	itemIndex int
}

// invocationResult is what runInvocation returns to its parent (loop / fan-
// out / runStage). path is the on-disk artifact path that was written.
// validators captures the final-turn validator results so loops can emit
// them as ExitFailures + sidecars can record them.
type invocationResult struct {
	path              string
	output            []byte
	validators        []icmtypes.ConditionResult
	convergenceFailed bool
	delegateOut       delegate.Output
	turnsUsed         int
}

// runInvocation runs a single sub-agent invocation: one turn (fixed) or
// many (until_valid / until_human_approves). Returns the on-disk path of
// the artifact written and the validators recorded against it. The
// orchestrator's dispatchStage wraps this for plain stages; runLoop /
// runFanOut wrap it for the more complex shapes.
func (o *Orchestrator) runInvocation(ctx context.Context, ic invocationCtx) (string, error) {
	res, err := o.runInvocationFull(ctx, ic)
	if err != nil {
		return "", err
	}
	return res.path, nil
}

// runInvocationFull is the rich-return variant used by runLoop /
// runFanOut. It exposes the validator failures and the underlying delegate
// output so callers can decide whether to iterate again or to write an
// aggregate.
func (o *Orchestrator) runInvocationFull(ctx context.Context, ic invocationCtx) (invocationResult, error) {
	stage := ic.stage
	turnsMax := stage.Turns.Max
	if turnsMax < 1 {
		turnsMax = 1
	}

	posture := o.PostureBuilder.PostureName(stage.ID)
	policy := stage.Turns.Policy
	if policy == "" {
		policy = workspace.TurnsFixed
	}

	evalCtxBase := predicates.StageEvalContext{
		RunID:                 o.RunID,
		StageID:               stage.ID,
		ItemID:                ic.itemID,
		Iteration:             ic.iteration,
		Container:             "output.validators",
		ParentTurnID:          o.ParentTurnID,
		ParentDepth:           o.ParentDepth,
		WorkspaceRoot:         o.Workflow.Root,
		StageBudgetTimeoutSec: stage.Agent.Budget.TimeoutSeconds,
		InstanceID:            o.InstanceID,
	}

	var (
		prevAttempt *PreviousAttempt
		lastResult  invocationResult
	)

	for turn := 1; turn <= turnsMax; turn++ {
		if err := ctx.Err(); err != nil {
			return invocationResult{}, err
		}

		payload, err := o.Payload.Build(PayloadInputs{
			Stage:           stage,
			Iteration:       ic.iteration,
			Turn:            turn,
			ItemID:          ic.itemID,
			ItemValue:       ic.itemValue,
			RunID:           o.RunID,
			PreviousAttempt: prevAttempt,
			PreviousIteration: previousIterationFromCtx(ic),
		})
		if err != nil {
			return invocationResult{}, fmt.Errorf("payload build: %w", err)
		}

		out, dispatchErr := o.Runtime.Run(ctx, delegate.Input{
			Posture:     posture,
			Task:        payload,
			Context:     nil,
			ParentTurn:  o.ParentTurnID,
			ParentDepth: o.ParentDepth + 1,
		})
		if dispatchErr != nil || isDelegateFailure(out) {
			cause := dispatchErr
			if cause == nil {
				cause = fmt.Errorf("delegate status %s: %s", out.Status, out.Error)
			}
			handled, finalArtifact, herr := o.handleDispatchError(ctx, stage, ic, turn, cause)
			if herr != nil {
				return invocationResult{}, herr
			}
			switch handled {
			case dispatchActionRetry:
				turn-- // retry does NOT advance the turn counter
				continue
			case dispatchActionFinalize:
				return finalArtifact, nil
			case dispatchActionHalt:
				return invocationResult{}, cause
			}
		}

		// Evaluate validators (output.validators) against the produced output.
		artifactBytes := []byte(out.Result)
		allPassed, results := o.Evaluator.EvaluateAll(ctx, stage.Output.Validators, artifactBytes, evalCtxBase)

		condResults := resultsToConditions(results)
		// Emit the per-turn icm.turn event with any failures from this turn.
		if o.Bus != nil {
			_ = o.Bus.Emit("icm.turn", icmtypes.ICMTurn{
				SchemaVersion: icmtypes.ICMTurnVersion,
				RunID:         o.RunID,
				StageID:       stage.ID,
				ItemID:        ic.itemID,
				Iteration:     ic.iteration,
				Turn:          turn,
				MaxTurns:      turnsMax,
				LastFailures:  failingConditions(condResults),
			})
		}

		// Record the stage state's turn counter.
		o.withState(func(_ *session.RunState) {
			if state := o.stageStateRef(stage.ID); state != nil {
				state.TurnCount = turn
			}
		})
		_ = o.saveState()

		lastResult = invocationResult{
			output:      artifactBytes,
			validators:  condResults,
			delegateOut: out,
			turnsUsed:   turn,
		}

		if allPassed {
			path, err := o.persistInvocation(stage, ic, artifactBytes, condResults, out, false)
			if err != nil {
				return invocationResult{}, err
			}
			lastResult.path = path
			return lastResult, nil
		}

		// Validators failed — decide whether to retry / hand off / ask human.
		switch policy {
		case workspace.TurnsFixed:
			// Fixed policy: persist this attempt as the artifact w/ a
			// convergence-failed marker and return.
			path, err := o.persistInvocation(stage, ic, artifactBytes, condResults, out, true)
			if err != nil {
				return invocationResult{}, err
			}
			lastResult.path = path
			lastResult.convergenceFailed = true
			return lastResult, nil

		case workspace.TurnsUntilValid:
			if turn >= turnsMax {
				// Budget exhausted: persist + return as handoff.
				path, err := o.persistInvocation(stage, ic, artifactBytes, condResults, out, true)
				if err != nil {
					return invocationResult{}, err
				}
				lastResult.path = path
				lastResult.convergenceFailed = true
				return lastResult, nil
			}
			// Set up retry feedback for the next turn.
			prevAttempt = &PreviousAttempt{
				Turn:     turn,
				Output:   artifactBytes,
				Failures: failingConditions(condResults),
			}

		case workspace.TurnsUntilHumanApproves:
			decision, free, err := o.runTurnHumanGate(ctx, stage, ic, turn)
			if err != nil {
				return invocationResult{}, err
			}
			switch decision {
			case turnDecisionAllow:
				path, err := o.persistInvocation(stage, ic, artifactBytes, condResults, out, false)
				if err != nil {
					return invocationResult{}, err
				}
				lastResult.path = path
				return lastResult, nil
			case turnDecisionContinue:
				if turn >= turnsMax {
					path, err := o.persistInvocation(stage, ic, artifactBytes, condResults, out, true)
					if err != nil {
						return invocationResult{}, err
					}
					lastResult.path = path
					lastResult.convergenceFailed = true
					return lastResult, nil
				}
				prevAttempt = &PreviousAttempt{
					Turn:          turn,
					Output:        artifactBytes,
					Failures:      failingConditions(condResults),
					HumanFeedback: free,
				}
			case turnDecisionReject:
				return invocationResult{}, fmt.Errorf("stage %s rejected at turn %d: %w", stage.ID, turn, errRejected)
			}

		default:
			return invocationResult{}, fmt.Errorf("unknown turn policy %q", policy)
		}
	}

	// Loop terminated without an explicit return — should be unreachable
	// because every policy branch returns inside the loop. Defensive halt.
	return lastResult, nil
}

// turnDecision enumerates the operator's choice at the per-turn HITL gate
// under turn_policy=until_human_approves.
type turnDecision int

const (
	turnDecisionAllow turnDecision = iota
	turnDecisionContinue
	turnDecisionReject
)

// runTurnHumanGate emits the icm.stage.turn HITL request and translates
// the operator's choice into a turnDecision. Cancelled / unrecognized
// responses surface as reject.
func (o *Orchestrator) runTurnHumanGate(ctx context.Context, stage *workspace.Stage, ic invocationCtx, turn int) (turnDecision, string, error) {
	if o.HITLDispatch == nil {
		// No HITL plumbed — treat as allow so tests without the HITL surface
		// don't deadlock.
		return turnDecisionAllow, "", nil
	}
	reqID := o.newHITLID("icm.stage.turn", stage.ID, fmt.Sprintf("t%d", turn))
	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              reqID,
		TurnID:          o.ParentTurnID,
		RequesterPlugin: o.InstanceID,
		ActionKind:      "icm.stage.turn",
		ActionRef: map[string]any{
			"run_id":    o.RunID,
			"stage_id":  stage.ID,
			"item_id":   ic.itemID,
			"iteration": ic.iteration,
			"turn":      turn,
		},
		Mode: events.HITLModeBoth,
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Accept", Kind: events.ChoiceAllow},
			{ID: "continue", Label: "Continue", Kind: events.ChoiceCustom},
			{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
		},
		Prompt: fmt.Sprintf("Turn %d of stage %s — accept, continue, or reject?", turn, stageLabel(stage)),
		Metadata: map[string]any{
			"icm.kind":   "stage_turn",
			"icm.run_id": o.RunID,
			"icm.stage":  stage.ID,
		},
	}
	resp, err := o.HITLDispatch(ctx, req)
	if err != nil {
		return turnDecisionReject, "", fmt.Errorf("stage turn gate: %w", err)
	}
	if resp.Cancelled {
		return turnDecisionReject, "", fmt.Errorf("stage turn gate cancelled: %s", resp.CancelReason)
	}
	switch resp.ChoiceID {
	case "allow":
		return turnDecisionAllow, resp.FreeText, nil
	case "continue":
		return turnDecisionContinue, resp.FreeText, nil
	case "reject":
		return turnDecisionReject, resp.FreeText, nil
	default:
		return turnDecisionReject, "", fmt.Errorf("stage turn gate: unknown choice %q", resp.ChoiceID)
	}
}

// dispatchAction enumerates the outcomes of handleDispatchError.
type dispatchAction int

const (
	dispatchActionHalt dispatchAction = iota
	dispatchActionRetry
	dispatchActionFinalize
)

// handleDispatchError applies the stage's error policy. retry asks the turn
// loop to redo the same turn; finalize returns a persisted artifact with a
// convergence-failed marker; halt returns the caller's error.
func (o *Orchestrator) handleDispatchError(ctx context.Context, stage *workspace.Stage, ic invocationCtx, turn int, cause error) (dispatchAction, invocationResult, error) {
	policy := o.errorPolicyFor(stage)
	switch policy {
	case workspace.ErrorRetry:
		return dispatchActionRetry, invocationResult{}, nil

	case workspace.ErrorHumanGate:
		if o.HITLDispatch == nil {
			return dispatchActionHalt, invocationResult{}, cause
		}
		reqID := o.newHITLID("icm.dispatch.error", stage.ID, fmt.Sprintf("t%d", turn))
		req := events.HITLRequest{
			SchemaVersion:   events.HITLRequestVersion,
			ID:              reqID,
			TurnID:          o.ParentTurnID,
			RequesterPlugin: o.InstanceID,
			ActionKind:      "icm.dispatch.error",
			ActionRef: map[string]any{
				"run_id":   o.RunID,
				"stage_id": stage.ID,
				"item_id":  ic.itemID,
				"turn":     turn,
				"error":    cause.Error(),
			},
			Mode: events.HITLModeBoth,
			Choices: []events.HITLChoice{
				{ID: "approve", Label: "Approve handoff", Kind: events.ChoiceAllow},
				{ID: "restart", Label: "Restart", Kind: events.ChoiceCustom},
				{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
			},
			Prompt: fmt.Sprintf("Stage %s dispatch failed: %v", stage.ID, cause),
			Metadata: map[string]any{
				"icm.kind":   "dispatch_error",
				"icm.run_id": o.RunID,
				"icm.stage":  stage.ID,
			},
		}
		resp, err := o.HITLDispatch(ctx, req)
		if err != nil {
			return dispatchActionHalt, invocationResult{}, fmt.Errorf("dispatch error gate: %w", err)
		}
		if resp.Cancelled {
			return dispatchActionHalt, invocationResult{}, fmt.Errorf("dispatch error gate cancelled: %s", resp.CancelReason)
		}
		switch resp.ChoiceID {
		case "approve":
			// Finalize an empty handoff artifact carrying the error in the
			// sidecar so downstream stages can decide whether to proceed.
			res := invocationResult{
				output:            []byte(""),
				convergenceFailed: true,
				validators: []icmtypes.ConditionResult{{
					Type: "dispatch_error", Verdict: "fail", Feedback: cause.Error(),
				}},
			}
			path, perr := o.persistInvocation(stage, ic, res.output, res.validators, delegate.Output{}, true)
			if perr != nil {
				return dispatchActionHalt, invocationResult{}, perr
			}
			res.path = path
			return dispatchActionFinalize, res, nil
		case "restart":
			return dispatchActionRetry, invocationResult{}, nil
		case "reject":
			return dispatchActionHalt, invocationResult{}, fmt.Errorf("dispatch error rejected: %w", errRejected)
		default:
			return dispatchActionHalt, invocationResult{}, fmt.Errorf("dispatch error gate: unknown choice %q", resp.ChoiceID)
		}

	default: // halt + unknown
		return dispatchActionHalt, invocationResult{}, cause
	}
}

// persistInvocation writes the artifact + sidecar at the appropriate path
// for this invocation's shape (plain / loop iteration / fan-out item /
// fan-out + loop iteration). Returns the absolute on-disk path written.
func (o *Orchestrator) persistInvocation(stage *workspace.Stage, ic invocationCtx, content []byte, validators []icmtypes.ConditionResult, out delegate.Output, convergenceFailed bool) (string, error) {
	var path string
	switch {
	case ic.itemID != "" && ic.iteration > 0:
		path = o.Session.ItemIterationArtifactPath(stage.ID, ic.itemID, stage.Output.Filename, ic.iteration)
	case ic.itemID != "":
		path = o.Session.ItemArtifactPath(stage.ID, ic.itemID, stage.Output.Filename)
	case ic.iteration > 0:
		path = o.Session.IterationArtifactPath(stage.ID, stage.Output.Filename, ic.iteration)
	default:
		path = o.Session.ArtifactPath(stage.ID, stage.Output.Filename)
	}

	if err := o.Session.WriteArtifact(path, content); err != nil {
		return "", err
	}

	meta := session.ArtifactMeta{
		StageID:           stage.ID,
		ConvergenceFailed: convergenceFailed,
		Delegate: session.DelegateMeta{
			PostureName:    out.PostureName,
			PostureVersion: out.PostureVer,
			TokensUsed:     out.TokensUsed,
			ToolCallsUsed:  out.ToolCallsUsed,
			ElapsedMS:      out.Elapsed.Milliseconds(),
		},
		ValidatorsPassed: passingConditions(validators),
		UnmetConditions:  failingConditions(validators),
		ParentArtifacts:  append([]string(nil), stage.Inputs.Artifacts...),
		WrittenAt:        time.Now().UTC(),
	}
	if ic.iteration > 0 {
		meta.IterationsRun = ic.iteration
	}
	if err := o.Session.WriteSidecar(path, meta); err != nil {
		return path, err
	}
	return path, nil
}

// previousIterationFromCtx returns the PreviousIteration block for the
// payload builder when the orchestrator is dispatching a loop iteration
// after iteration 1. Returns nil when there is no prior iteration to
// surface.
func previousIterationFromCtx(ic invocationCtx) *PreviousIteration {
	if ic.prevIterIdx == 0 || len(ic.prevIterArt) == 0 {
		return nil
	}
	return &PreviousIteration{
		Index:    ic.prevIterIdx,
		Artifact: ic.prevIterArt,
		Path:     ic.prevIterPath,
		Failures: ic.prevIterFails,
	}
}

// resultsToConditions converts evaluator results into the serialized
// ConditionResult shape used in event payloads + sidecars.
func resultsToConditions(rs []predicates.Result) []icmtypes.ConditionResult {
	out := make([]icmtypes.ConditionResult, 0, len(rs))
	for _, r := range rs {
		verdict := "fail"
		if r.Verdict {
			verdict = "pass"
		}
		out = append(out, icmtypes.ConditionResult{
			Type:     string(r.Type),
			Name:     r.Name,
			Verdict:  verdict,
			Feedback: r.Feedback,
			Score:    r.Score,
		})
	}
	return out
}

// failingConditions filters the input to entries with Verdict != "pass".
func failingConditions(cs []icmtypes.ConditionResult) []icmtypes.ConditionResult {
	var out []icmtypes.ConditionResult
	for _, c := range cs {
		if c.Verdict != "pass" {
			out = append(out, c)
		}
	}
	return out
}

// passingConditions filters the input to entries with Verdict == "pass".
func passingConditions(cs []icmtypes.ConditionResult) []icmtypes.ConditionResult {
	var out []icmtypes.ConditionResult
	for _, c := range cs {
		if c.Verdict == "pass" {
			out = append(out, c)
		}
	}
	return out
}

// isDelegateFailure reports whether a delegate output should be treated as
// a dispatch error: explicit error / timeout / cancellation. Partial
// outputs are NOT treated as failures — they may still carry usable
// content the validators can grade.
func isDelegateFailure(out delegate.Output) bool {
	switch out.Status {
	case delegate.StatusError, delegate.StatusTimeout, delegate.StatusCancel:
		return true
	}
	return false
}

