// Package runtime — top-level ICM orchestrator.
//
// The Orchestrator drives a single workflow run from the first stage through
// the last. It owns no LLM client, no HITL surface, and no session storage
// directly: every sub-agent dispatch routes through a Dispatcher (typically
// *delegate.Runtime), every operator interaction routes through HITLDispatch
// (supplied by the plugin), and every artifact / state write is funneled
// through a *session.Session. Tests substitute fakes for the Dispatcher and
// HITLDispatch fields to exercise control flow without touching the LLM
// surface.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// Dispatcher is the narrow contract the orchestrator uses to invoke a
// sub-agent. *delegate.Runtime satisfies this interface; tests substitute
// a fake that returns canned outputs without touching the LLM surface.
type Dispatcher interface {
	Run(ctx context.Context, in delegate.Input) (delegate.Output, error)
}

// HITLDispatchFunc is the orchestrator-facing HITL gateway. The plugin wires
// it to its own emit-and-wait machinery (predicates_wiring.go) so the
// orchestrator never touches the bus directly for HITL traffic.
type HITLDispatchFunc func(ctx context.Context, req events.HITLRequest) (events.HITLResponse, error)

// NewHITLIDFunc generates a unique HITL request ID. The plugin's newHITLID
// helper satisfies this; the orchestrator never invents IDs itself so the
// hitl-prefix filter in handleHITLResponded stays the sole source of truth.
type NewHITLIDFunc func(kind, runID, stageID, extra string) string

// Orchestrator runs a single ICM workflow end-to-end. Construct via the
// plugin (Plugin.buildOrchestrator) — direct construction is allowed in
// tests but production code should funnel through the plugin so every
// field is populated.
type Orchestrator struct {
	Workflow       *workspace.Workflow
	Session        *session.Session
	Runtime        Dispatcher
	Evaluator      *predicates.Evaluator
	Payload        *PayloadBuilder
	PostureBuilder *PostureBuilder
	Bus            engine.EventBus
	Logger         *slog.Logger

	// HITLDispatch dispatches a HITL request and waits for the response.
	// nil disables every human-gate path: gates auto-resolve as allow and
	// HITL-backed predicates surface as failures.
	HITLDispatch HITLDispatchFunc
	// NewHITLID supplies request IDs. Required when HITLDispatch is set.
	NewHITLID NewHITLIDFunc

	// InstanceID identifies the plugin instance owning this run; surfaces
	// in event payloads + StageEvalContext.
	InstanceID string
	// RunID identifies the run. Mirrors Session.RunID; carried separately
	// so event payloads can be assembled before the session loads.
	RunID string
	// ParentTurnID is the upstream turn that triggered the run, propagated
	// for causation.
	ParentTurnID string
	// ParentDepth is the sub-agent depth of the upstream turn.
	ParentDepth int
	// LoopMaxRestarts caps how many times a single looping stage may be
	// restarted via the human_gate exhaustion path. 0 disables the cap.
	LoopMaxRestarts int
	// EmitThinkingSteps mirrors the plugin's emit_progress_thinking_steps
	// option. Currently unused by the orchestrator surface; reserved for
	// future thinking-step emission per stage transition.
	EmitThinkingSteps bool

	// stateMu serializes mutations to state across fan-out goroutines.
	// SaveState (and any read followed by mutation) must hold this lock.
	stateMu sync.Mutex
	state   *session.RunState
}

// Run drives the workflow from first stage to last. Returns a non-nil error
// on any halt path (dispatch error policy halt, rejected gate, loop
// exhausted under on_exhausted=error, cancelled context). The run is
// always reported via either ICMRunCompleted or ICMRunHalted before
// return.
func (o *Orchestrator) Run(ctx context.Context) error {
	if err := o.preflight(); err != nil {
		return err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}

	startedAt := time.Now().UTC()
	if err := o.initState(startedAt); err != nil {
		return err
	}

	o.emitRunStarted()
	o.emitWorkflowProgress(events.WorkflowProgress{
		Status: events.WorkflowStatusStarted,
		Detail: "workflow started",
	})

	for i := range o.Workflow.Stages {
		stage := &o.Workflow.Stages[i]
		o.state.CurrentStage = i

		if err := ctx.Err(); err != nil {
			return o.haltRun(stage.ID, "cancelled: "+err.Error(), true, startedAt, err)
		}

		if err := o.runStage(ctx, stage, i); err != nil {
			return o.haltRun(stage.ID, err.Error(), errors.Is(err, context.Canceled), startedAt, err)
		}
	}

	// All stages completed cleanly.
	o.state.Outcome = session.OutcomeCompleted
	o.state.CurrentStage = len(o.Workflow.Stages)
	_ = o.saveState()

	elapsed := int64(time.Since(startedAt).Seconds())
	if o.Bus != nil {
		_ = o.Bus.Emit("icm.run.completed", icmtypes.ICMRunCompleted{
			SchemaVersion:  icmtypes.ICMRunCompletedVersion,
			RunID:          o.RunID,
			StagesRun:      len(o.Workflow.Stages),
			ElapsedSeconds: elapsed,
		})
		o.emitWorkflowProgress(events.WorkflowProgress{
			Status: events.WorkflowStatusCompleted,
			Detail: "workflow completed",
		})
	}
	return nil
}

// preflight asserts the orchestrator was wired before Run executes.
func (o *Orchestrator) preflight() error {
	if o.Workflow == nil {
		return errors.New("orchestrator: workflow is required")
	}
	if o.Session == nil {
		return errors.New("orchestrator: session is required")
	}
	if o.Runtime == nil {
		return errors.New("orchestrator: dispatcher is required")
	}
	if o.Evaluator == nil {
		return errors.New("orchestrator: evaluator is required")
	}
	if o.Payload == nil {
		return errors.New("orchestrator: payload builder is required")
	}
	if o.PostureBuilder == nil {
		return errors.New("orchestrator: posture builder is required")
	}
	if o.RunID == "" {
		o.RunID = o.Session.RunID
	}
	return nil
}

// initState loads existing state (zero-value when none) and seeds it for
// this run. Each stage is recorded up-front so external observers see the
// full execution plan.
func (o *Orchestrator) initState(startedAt time.Time) error {
	st, err := o.Session.LoadState()
	if err != nil {
		return fmt.Errorf("orchestrator: load state: %w", err)
	}
	if st.RunID == "" {
		st.RunID = o.RunID
	}
	st.InstanceID = o.InstanceID
	st.WorkspaceRoot = o.Workflow.Root
	st.StartedAt = startedAt
	st.Outcome = session.OutcomeRunning
	st.Stages = make([]session.StageState, len(o.Workflow.Stages))
	for i, stage := range o.Workflow.Stages {
		st.Stages[i] = session.StageState{
			ID:     stage.ID,
			Status: session.StageStatusPending,
		}
	}
	o.state = st
	return o.saveState()
}

// haltRun records the halt in state, emits ICMRunHalted, and returns the
// supplied cause unchanged so the caller's error wrapping survives.
func (o *Orchestrator) haltRun(stageID, reason string, cancelled bool, startedAt time.Time, cause error) error {
	if o.state != nil {
		if cancelled {
			o.state.Outcome = session.OutcomeCancelled
		} else if isRejectError(cause) {
			o.state.Outcome = session.OutcomeRejected
		} else {
			o.state.Outcome = session.OutcomeHalted
		}
		_ = o.saveState()
	}
	if o.Bus != nil {
		_ = o.Bus.Emit("icm.run.halted", icmtypes.ICMRunHalted{
			SchemaVersion:  icmtypes.ICMRunHaltedVersion,
			RunID:          o.RunID,
			Reason:         reason,
			HaltedAtStage:  stageID,
			Cancelled:      cancelled,
			ElapsedSeconds: int64(time.Since(startedAt).Seconds()),
		})
		o.emitWorkflowProgress(events.WorkflowProgress{
			Stage:  stageID,
			Status: events.WorkflowStatusHalted,
			Detail: reason,
		})
	}
	return cause
}

// emitRunStarted publishes the ICMRunStarted event. No-op when Bus is nil.
func (o *Orchestrator) emitRunStarted() {
	if o.Bus == nil {
		return
	}
	name := lastPathElement(o.Workflow.Root)
	_ = o.Bus.Emit("icm.run.started", icmtypes.ICMRunStarted{
		SchemaVersion: icmtypes.ICMRunStartedVersion,
		RunID:         o.RunID,
		InstanceID:    o.InstanceID,
		WorkspaceRoot: o.Workflow.Root,
		WorkspaceName: name,
		Stages:        len(o.Workflow.Stages),
	})
}

// saveState writes the orchestrator's RunState through the session helper.
// Errors are logged but otherwise non-fatal: state.json is observational.
// The caller is responsible for holding stateMu around the read+mutate+save
// pattern; this method also locks for the duration of the marshal to make
// a coherent snapshot even when called without an outer hold.
func (o *Orchestrator) saveState() error {
	if o.state == nil || o.Session == nil {
		return nil
	}
	o.stateMu.Lock()
	snapshot := cloneState(o.state)
	o.stateMu.Unlock()
	if err := o.Session.SaveState(snapshot); err != nil {
		o.Logger.Warn("icm.orchestrator: save state", "run_id", o.RunID, "err", err)
		return err
	}
	return nil
}

// withState invokes fn with stateMu held. Use this whenever mutating
// o.state from a goroutine that may race with other fan-out workers.
func (o *Orchestrator) withState(fn func(*session.RunState)) {
	o.stateMu.Lock()
	defer o.stateMu.Unlock()
	fn(o.state)
}

// stageStateRef returns a mutable pointer into o.state.Stages keyed by ID,
// or nil when the ID is not a primary stage. Verifiers run through the same
// runInvocation surface as stages but their state lives in o.state.Verifiers
// rather than o.state.Stages, so a verifier ID is a legitimate miss here.
// Callers that mutate through the returned pointer in a goroutine must wrap
// the access in withState; sequential callers (top-level Run, runStage,
// runLoop) can rely on the in-order semantics of the surrounding code.
//
// Stage-only callers (runStage, runLoop, runFanOut, runOutput) expect a
// non-nil return — those paths never touch verifiers, so a nil here would
// indicate a programming error and the subsequent nil-deref is the
// intended fail-fast.
func (o *Orchestrator) stageStateRef(stageID string) *session.StageState {
	for i := range o.state.Stages {
		if o.state.Stages[i].ID == stageID {
			return &o.state.Stages[i]
		}
	}
	return nil
}

// cloneState returns a deep-enough copy of the RunState for marshalling.
// The JSON marshaller walks pointers and slices, so the simplest defense
// against the SaveState/goroutine race is a shallow snapshot of the
// stages + verifiers tables that the marshaller can iterate without
// contention.
func cloneState(in *session.RunState) *session.RunState {
	if in == nil {
		return nil
	}
	out := *in
	if in.Stages != nil {
		stages := make([]session.StageState, len(in.Stages))
		for i, st := range in.Stages {
			stages[i] = st
			if st.Iterations != nil {
				its := make([]session.IterationState, len(st.Iterations))
				copy(its, st.Iterations)
				stages[i].Iterations = its
			}
			if st.Items != nil {
				items := make([]session.ItemState, len(st.Items))
				copy(items, st.Items)
				stages[i].Items = items
			}
		}
		out.Stages = stages
	}
	if in.Verifiers != nil {
		v := make(map[string]session.VerifierState, len(in.Verifiers))
		for k, vs := range in.Verifiers {
			v[k] = vs
		}
		out.Verifiers = v
	}
	return &out
}

// ---------------------------------------------------------------------------
// Internal sentinel errors
// ---------------------------------------------------------------------------

// errRejected is returned by gate / dispatch paths when a human-gate /
// validator failure surfaces the rejection outcome. Run() converts this to
// the "rejected" run outcome rather than the generic "halted" one.
var errRejected = errors.New("orchestrator: rejected")

// isRejectError reports whether err originated from a HITL reject choice.
func isRejectError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errRejected)
}

// ---------------------------------------------------------------------------
// Small helpers shared by stage / loop / fanout files
// ---------------------------------------------------------------------------

// lastPathElement returns the basename of an absolute path; used to derive
// a workspace name from its root folder.
func lastPathElement(p string) string {
	if p == "" {
		return ""
	}
	// Avoid importing path/filepath just for this — orchestrator already
	// imports it via session, but the helper is tiny enough to inline.
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}

// errorPolicyFor resolves the effective error policy for a stage, falling
// back to workspace defaults when the stage left it unset.
func (o *Orchestrator) errorPolicyFor(stage *workspace.Stage) workspace.ErrorPolicy {
	if stage.OnError != "" {
		return stage.OnError
	}
	if o.Workflow.Defaults.OnError != "" {
		return o.Workflow.Defaults.OnError
	}
	return workspace.ErrorHalt
}

// humanGateFor resolves the effective human-gate position for a stage.
func (o *Orchestrator) humanGateFor(stage *workspace.Stage) workspace.HumanGate {
	if stage.HumanGate != "" {
		return stage.HumanGate
	}
	if o.Workflow.Defaults.HumanGate != "" {
		return o.Workflow.Defaults.HumanGate
	}
	return workspace.HumanGateNone
}
