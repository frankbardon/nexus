// Package predicates implements the ICM workflow predicate evaluator.
//
// The evaluator dispatches Predicate values produced by the workspace
// loader against an artifact (the bytes a stage emitted) and returns a
// Result per predicate. Six predicate types are supported: schema,
// regex, native, command, llm, human. Each type has a dedicated
// per-file evaluator in this package.
//
// Schema, regex, and command are implemented end-to-end. Native
// dispatches to a process-local handler registry; an unregistered
// handler reports a non-fatal failure result. LLM and human predicates
// invoke pluggable dispatchers (JudgeDispatch / HumanDispatch); when no
// dispatcher is configured, evaluation returns a failure result with an
// explanatory feedback string. Wiring those dispatchers happens in
// later commits.
package predicates

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	jsc "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// Result is the unified outcome of evaluating a single predicate.
// Verdict=true means the artifact satisfied the predicate; Verdict=false
// means it did not, in which case Feedback should describe why.
type Result struct {
	// Name mirrors the predicate's Name field for stable identification
	// in event payloads and feedback strings.
	Name string
	// Type echoes the predicate's discriminator so callers can route
	// results without re-reading the original Predicate.
	Type workspace.PredicateType
	// Verdict is true on pass, false on fail.
	Verdict bool
	// Feedback explains a failure (or, optionally, a pass). Empty
	// strings on failing results are still legal; downstream code falls
	// back to a generic message.
	Feedback string
	// Score is an optional 0..1 confidence number set by LLM judges.
	Score *float64
	// Elapsed is the wall time the evaluator spent on this predicate.
	Elapsed time.Duration
}

// StageEvalContext carries the per-stage values the evaluator needs to
// dispatch certain predicate types. Built by the orchestrator once per
// stage invocation and passed through to every Evaluate call for that
// stage.
type StageEvalContext struct {
	// RunID is the run that owns this evaluation.
	RunID string
	// StageID is the stage being evaluated.
	StageID string
	// ItemID names the fan-out item, when applicable.
	ItemID string
	// Iteration is the 1-based loop iteration index, when applicable.
	Iteration int
	// Container labels what surface invoked the evaluator. Standard
	// values: "output.validators", "loop.until", "verifier".
	Container string
	// ParentTurnID is the dispatching turn's ID, propagated for
	// causation tracking by downstream predicate types that surface to
	// LLM judges or human handlers.
	ParentTurnID string
	// ParentDepth is the sub-agent depth of the dispatching turn.
	ParentDepth int
	// WorkspaceRoot is the absolute root path of the workspace, used by
	// command predicates to resolve relative script paths.
	WorkspaceRoot string
	// StageBudgetTimeoutSec is the stage's per-call timeout budget in
	// seconds (0 = no stage-level override). Command predicates consult
	// this when their own TimeoutSeconds is zero.
	StageBudgetTimeoutSec int
	// InstanceID names the icm plugin instance owning this run.
	// PredicateSchemaName uses it to derive the schema-registry name.
	InstanceID string
}

// NativeResult is what NativeHandler implementations return. The
// evaluator copies these fields into a Result and overwrites Name/Type
// with the predicate's declared values so handlers cannot accidentally
// rename themselves in event payloads.
type NativeResult struct {
	Verdict  bool
	Feedback string
	Score    *float64
}

// NativeHandler is the contract for in-process predicate handlers
// registered via Evaluator.RegisterNative. Step 5 of the ICM plugin
// build wires the real handlers; the skeleton ships an empty registry.
type NativeHandler interface {
	// Evaluate runs the handler against the given artifact and returns
	// its outcome. Handlers should be cheap and non-blocking — long
	// work belongs in command or llm predicates.
	Evaluate(ctx context.Context, args map[string]any, artifact []byte) NativeResult
}

// HumanDispatch is invoked by human predicates. The dispatcher prompts
// the operator (typically via the HITL plugin) and returns their
// verdict. Returning a non-nil error is treated as a failure result;
// the error message becomes the feedback.
type HumanDispatch func(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext) (verdict bool, feedback string, err error)

// JudgeDispatch is invoked by llm predicates. The dispatcher calls
// whichever judge model the predicate (or workspace default) requests
// and returns the parsed verdict. Returning a non-nil error becomes a
// failing result with the error message as feedback.
type JudgeDispatch func(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext) (verdict bool, feedback string, score *float64, err error)

// Evaluator runs Predicate values against artifacts. Construct via
// NewEvaluator and configure pluggable dispatchers via the public
// fields before use.
type Evaluator struct {
	// Schemas is the engine schema registry consulted for `type:
	// schema` predicates. Required; schema predicates without a
	// configured registry return a failure result.
	Schemas *engine.SchemaRegistry
	// Sandbox executes `type: command` predicates. Required for command
	// predicates; absent backend yields a failure result.
	Sandbox sandbox.Sandbox
	// Bus, when non-nil, receives an `icm.predicate.failed` event for
	// every failing predicate. Tests can leave this nil to skip
	// emission.
	Bus engine.EventBus
	// Logger records evaluation events. Must be non-nil.
	Logger *slog.Logger
	// Judge dispatches `type: llm` predicates. Nil until wired by the
	// plugin (step 6); evaluator returns "judge not configured" until
	// then.
	Judge JudgeDispatch
	// Human dispatches `type: human` predicates. Nil until wired by the
	// plugin (step 6); evaluator returns "human handler not configured"
	// until then.
	Human HumanDispatch
	// CommandTimeoutSecs is the plugin-config default applied to
	// command predicates whose own TimeoutSeconds and whose
	// StageBudgetTimeoutSec are both zero. Falls back to 30s when this
	// is also zero.
	CommandTimeoutSecs int

	nativeMu       sync.RWMutex
	nativeHandlers map[string]NativeHandler

	schemaCacheMu      sync.Mutex
	schemaCompileCache map[string]*jsc.Schema
}

// NewEvaluator constructs an Evaluator with the required dependencies.
// Dispatchers (Judge, Human) and config knobs (CommandTimeoutSecs) can
// be set directly on the returned value before use.
func NewEvaluator(schemas *engine.SchemaRegistry, sb sandbox.Sandbox, bus engine.EventBus, logger *slog.Logger) *Evaluator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Evaluator{
		Schemas:            schemas,
		Sandbox:            sb,
		Bus:                bus,
		Logger:             logger,
		nativeHandlers:     make(map[string]NativeHandler),
		schemaCompileCache: make(map[string]*jsc.Schema),
	}
}

// RegisterNative records h as the handler for name. Subsequent
// registrations under the same name replace the prior handler.
func (e *Evaluator) RegisterNative(name string, h NativeHandler) {
	e.nativeMu.Lock()
	defer e.nativeMu.Unlock()
	e.nativeHandlers[name] = h
}

// LookupNative returns the registered handler for name, if any.
func (e *Evaluator) LookupNative(name string) (NativeHandler, bool) {
	e.nativeMu.RLock()
	defer e.nativeMu.RUnlock()
	h, ok := e.nativeHandlers[name]
	return h, ok
}

// Evaluate runs a single predicate. The returned Result always has its
// Name, Type, and Elapsed fields populated regardless of outcome.
func (e *Evaluator) Evaluate(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext) Result {
	start := time.Now()
	res := Result{Name: p.Name, Type: p.Type}
	switch p.Type {
	case workspace.PredSchema:
		res = e.evalSchema(p, artifact, sc, res)
	case workspace.PredRegex:
		res = e.evalRegex(p, artifact, res)
	case workspace.PredCommand:
		res = e.evalCommand(ctx, p, artifact, sc, res)
	case workspace.PredNative:
		res = e.evalNative(ctx, p, artifact, res)
	case workspace.PredLLM:
		res = e.evalLLM(ctx, p, artifact, sc, res)
	case workspace.PredHuman:
		res = e.evalHuman(ctx, p, artifact, sc, res)
	default:
		res.Verdict = false
		res.Feedback = fmt.Sprintf("unknown predicate type %q", p.Type)
	}
	res.Elapsed = time.Since(start)
	return res
}

// EvaluateAll runs predicates in declared order, short-circuiting on
// the first failure. Returns allPassed=true only when every predicate
// returned Verdict=true. When Bus is non-nil, every failing predicate
// emits an icm.predicate.failed event.
func (e *Evaluator) EvaluateAll(ctx context.Context, ps []workspace.Predicate, artifact []byte, sc StageEvalContext) (allPassed bool, results []Result) {
	allPassed = true
	for i := range ps {
		r := e.Evaluate(ctx, &ps[i], artifact, sc)
		results = append(results, r)
		if !r.Verdict {
			allPassed = false
			e.emitFailure(&ps[i], r, sc)
			return allPassed, results
		}
	}
	return allPassed, results
}

// emitFailure publishes an icm.predicate.failed event when Bus is set.
// Errors emitting are logged but do not affect the evaluation outcome.
func (e *Evaluator) emitFailure(p *workspace.Predicate, r Result, sc StageEvalContext) {
	if e.Bus == nil {
		return
	}
	payload := icmtypes.ICMPredicateFailed{
		SchemaVersion: icmtypes.ICMPredicateFailedVersion,
		RunID:         sc.RunID,
		StageID:       sc.StageID,
		ItemID:        sc.ItemID,
		Container:     sc.Container,
		PredicateName: p.Name,
		PredicateType: string(p.Type),
		Feedback:      r.Feedback,
	}
	if err := e.Bus.Emit("icm.predicate.failed", payload); err != nil {
		e.Logger.Warn("predicates: emit icm.predicate.failed failed",
			"stage", sc.StageID, "predicate", p.Name, "err", err)
	}
}

// PredicateSchemaName returns the canonical schema-registry name for a
// predicate. The icm plugin uses this helper at Ready() to register
// every output schema; the evaluator uses it at lookup time. Both
// halves must agree on the format — keep changes in lockstep.
//
// Format: "icm.<instanceID>.<stageID>.<predName>". When predName is
// "output" (the synthesized stage-output schema predicate) the result
// still includes it, so output schemas and named schema predicates
// share one namespace.
func PredicateSchemaName(instanceID, stageID, predName string) string {
	return "icm." + instanceID + "." + stageID + "." + predName
}
