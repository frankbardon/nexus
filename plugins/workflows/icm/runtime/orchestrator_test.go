package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeDispatcher is a Dispatcher that returns canned outputs in order from
// the supplied slice. Errors take precedence over outputs at the same
// index.
type fakeDispatcher struct {
	mu      sync.Mutex
	calls   []delegate.Input
	outputs []delegate.Output
	errs    []error
	index   int
}

func (f *fakeDispatcher) Run(_ context.Context, in delegate.Input) (delegate.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.index
	f.index++
	f.calls = append(f.calls, in)
	var out delegate.Output
	if idx < len(f.outputs) {
		out = f.outputs[idx]
	}
	var err error
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	if out.Status == "" && err == nil {
		out.Status = delegate.StatusSuccess
	}
	return out, err
}

func (f *fakeDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.index
}

// fakeHITL is a HITL dispatcher that returns canned responses keyed by
// ActionKind. Each call pops the next response from the per-kind slice.
type fakeHITL struct {
	mu        sync.Mutex
	responses map[string][]events.HITLResponse
	calls     map[string]int
	seenReqs  []events.HITLRequest
}

func newFakeHITL() *fakeHITL {
	return &fakeHITL{
		responses: make(map[string][]events.HITLResponse),
		calls:     make(map[string]int),
	}
}

func (f *fakeHITL) queue(kind string, resp events.HITLResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[kind] = append(f.responses[kind], resp)
}

func (f *fakeHITL) dispatch(_ context.Context, req events.HITLRequest) (events.HITLResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[req.ActionKind]++
	f.seenReqs = append(f.seenReqs, req)
	queue := f.responses[req.ActionKind]
	if len(queue) == 0 {
		// Default to "allow" so tests that never wire a queue for a kind
		// still proceed.
		return events.HITLResponse{RequestID: req.ID, ChoiceID: "allow"}, nil
	}
	resp := queue[0]
	f.responses[req.ActionKind] = queue[1:]
	if resp.RequestID == "" {
		resp.RequestID = req.ID
	}
	return resp, nil
}

// fakeHITLID returns a deterministic-ish ID prefixed with "icm-" so any
// downstream filter that requires the prefix is satisfied.
func fakeHITLID(kind, runID, stageID, extra string) string {
	return "icm-" + kind + "-" + runID + "-" + stageID + "-" + extra + "-fake"
}

// silentLogger returns a slog logger that discards everything.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------------------------------------------------------------------------
// Workflow builder
// ---------------------------------------------------------------------------

// workflowOpt mutates a stage during workflowBuilder.add().
type workflowOpt func(*workspace.Stage)

func withTurnPolicy(policy workspace.TurnPolicy, max int) workflowOpt {
	return func(s *workspace.Stage) {
		s.Turns.Policy = policy
		s.Turns.Max = max
	}
}

func withHumanGate(g workspace.HumanGate) workflowOpt {
	return func(s *workspace.Stage) { s.HumanGate = g }
}

func withOnError(p workspace.ErrorPolicy) workflowOpt {
	return func(s *workspace.Stage) { s.OnError = p }
}

func withValidator(pred workspace.Predicate) workflowOpt {
	return func(s *workspace.Stage) {
		s.Output.Validators = append(s.Output.Validators, pred)
	}
}

func withLoop(max int, until []workspace.Predicate, onExhausted workspace.ExhaustedAction) workflowOpt {
	return func(s *workspace.Stage) {
		s.Loop = &workspace.LoopConfig{MaxIterations: max, Until: until, OnExhausted: onExhausted}
	}
}

func withFanOut(source string, parallel int, policy workspace.ItemFailureAction) workflowOpt {
	return func(s *workspace.Stage) {
		s.FanOut = &workspace.FanOutConfig{
			Source:        source,
			ItemVar:       "item",
			MaxParallel:   parallel,
			OnItemFailure: policy,
		}
	}
}

// orchTestFixture bundles the artifacts a test orchestrator needs.
type orchTestFixture struct {
	workflow  *workspace.Workflow
	session   *session.Session
	disp      *fakeDispatcher
	hitl      *fakeHITL
	evaluator *predicates.Evaluator
	bus       engine.EventBus
	postureB  *PostureBuilder
	payloadB  *PayloadBuilder
	orch      *Orchestrator
}

// newFixture constructs a basic fixture with one stage. Tests extend it
// via addStage().
func newFixture(t *testing.T, stageIDs ...string) *orchTestFixture {
	t.Helper()
	return newFixtureOpts(t, stageIDs, nil)
}

// stageDef captures a stage's ID + its options for newFixtureOpts.
type stageDef struct {
	id   string
	opts []workflowOpt
}

func newFixtureOpts(t *testing.T, simpleIDs []string, defs []stageDef) *orchTestFixture {
	t.Helper()
	tmp := t.TempDir()
	wsRoot := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(filepath.Join(wsRoot, "shared", "grounding"), 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}

	wf := &workspace.Workflow{
		Root: wsRoot,
		Operator: workspace.OperatorConfig{
			Body:   "Operator for {{ .Stage.ID }}.",
			Source: "default",
		},
		WorkspaceDoc: "Test workflow",
		Verifiers:    map[string]*workspace.Stage{},
	}
	for _, id := range simpleIDs {
		wf.Stages = append(wf.Stages, makeStage(id))
	}
	for _, def := range defs {
		stage := makeStage(def.id)
		for _, opt := range def.opts {
			opt(&stage)
		}
		wf.Stages = append(wf.Stages, stage)
	}

	dataDir := filepath.Join(tmp, "data")
	sess, err := session.NewSession(dataDir, "run_test", silentLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	reg := posture.NewRegistry()
	postureB := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   reg,
	}
	// Register a base posture per stage so the dispatcher receives a
	// known name. The fake dispatcher ignores the posture but we wire
	// the registry for parity with production.
	for i := range wf.Stages {
		ap, err := postureB.Build(&wf.Stages[i])
		if err != nil {
			t.Fatalf("Build posture: %v", err)
		}
		if err := reg.Register(ap); err != nil {
			t.Fatalf("Register posture: %v", err)
		}
	}

	bus := engine.NewEventBus()
	logger := silentLogger()
	ev := predicates.NewEvaluator(engine.NewSchemaRegistry(logger), nil, bus, logger)

	pb := &PayloadBuilder{Workflow: wf, Session: sess, Logger: logger}
	disp := &fakeDispatcher{}
	hitl := newFakeHITL()

	o := &Orchestrator{
		Workflow:       wf,
		Session:        sess,
		Runtime:        disp,
		Evaluator:      ev,
		Payload:        pb,
		PostureBuilder: postureB,
		Bus:            bus,
		Logger:         logger,
		HITLDispatch:   hitl.dispatch,
		NewHITLID:      fakeHITLID,
		InstanceID:     "nexus.workflows.icm",
		RunID:          "run_test",
	}
	return &orchTestFixture{
		workflow:  wf,
		session:   sess,
		disp:      disp,
		hitl:      hitl,
		evaluator: ev,
		bus:       bus,
		postureB:  postureB,
		payloadB:  pb,
		orch:      o,
	}
}

func makeStage(id string) workspace.Stage {
	return workspace.Stage{
		ID:      id,
		Display: id,
		Role:    "role of " + id,
		Turns:   workspace.TurnConfig{Policy: workspace.TurnsFixed, Max: 1},
		Output: workspace.OutputSpec{
			Format:   workspace.OutputText,
			Filename: id + ".md",
			Persist:  workspace.PersistFileRef,
		},
	}
}


// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// 1. Happy path: three plain stages, all complete without validators.
func TestOrchestrator_HappyPath(t *testing.T) {
	f := newFixture(t, "01_a", "02_b", "03_c")
	f.disp.outputs = []delegate.Output{
		{Result: "A output", Status: delegate.StatusSuccess},
		{Result: "B output", Status: delegate.StatusSuccess},
		{Result: "C output", Status: delegate.StatusSuccess},
	}

	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.disp.callCount() != 3 {
		t.Fatalf("dispatcher call count = %d, want 3", f.disp.callCount())
	}
	// Each stage produced its artifact.
	for _, id := range []string{"01_a", "02_b", "03_c"} {
		p := f.session.ArtifactPath(id, id+".md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("artifact %s missing: %v", p, err)
		}
	}
	if got := f.orch.state.Outcome; got != session.OutcomeCompleted {
		t.Errorf("Outcome = %q, want %q", got, session.OutcomeCompleted)
	}
}

// 2. Until-valid turn loop: first turn fails validator, second turn
// passes. One stage, two turns.
func TestOrchestrator_TurnsUntilValid_PassesOnRetry(t *testing.T) {
	pred := regexPred("must_have_ok", "OK")
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{
			withTurnPolicy(workspace.TurnsUntilValid, 3),
			withValidator(pred),
		}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "bad", Status: delegate.StatusSuccess},
		{Result: "all OK here", Status: delegate.StatusSuccess},
	}
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", got)
	}
}

// 3. Until-valid exhausts budget: every turn fails, artifact lands with
// convergence-failed marker but Run still returns nil.
func TestOrchestrator_TurnsUntilValid_Exhausts(t *testing.T) {
	pred := regexPred("must_have_ok", "OK")
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{
			withTurnPolicy(workspace.TurnsUntilValid, 2),
			withValidator(pred),
		}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "bad", Status: delegate.StatusSuccess},
		{Result: "still bad", Status: delegate.StatusSuccess},
	}
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Sidecar should mark convergence_failed.
	sidecar := session.SidecarPath(f.session.ArtifactPath("01_a", "01_a.md"))
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(data), `"convergence_failed": true`) {
		t.Errorf("sidecar missing convergence_failed marker: %s", data)
	}
}

// 4. Dispatch error with retry policy: first call errors, retry succeeds.
func TestOrchestrator_DispatchError_Retry(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withOnError(workspace.ErrorRetry)}},
	})
	f.disp.outputs = []delegate.Output{
		{Status: delegate.StatusError, Error: "boom"},
		{Result: "OK", Status: delegate.StatusSuccess},
	}
	f.disp.errs = []error{errors.New("provider down"), nil}
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", got)
	}
}

// 5. Dispatch error with halt policy: first call errors, run halts.
func TestOrchestrator_DispatchError_Halt(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withOnError(workspace.ErrorHalt)}},
	})
	f.disp.errs = []error{errors.New("provider down")}
	f.disp.outputs = []delegate.Output{{Status: delegate.StatusError, Error: "provider down"}}
	if err := f.orch.Run(context.Background()); err == nil {
		t.Fatal("Run: expected error, got nil")
	}
	if got := f.orch.state.Outcome; got != session.OutcomeHalted {
		t.Errorf("Outcome = %q, want %q", got, session.OutcomeHalted)
	}
}

// 6. Loop converges on iteration 3 via loop.until.
func TestOrchestrator_LoopConverges(t *testing.T) {
	until := workspace.Predicate{
		Type:    workspace.PredRegex,
		Name:    "looks_done",
		Pattern: "DONE",
		Anchor:  workspace.AnchorWhole,
	}
	until.SetCompiledRegex(mustRegex("DONE"))

	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_loop", opts: []workflowOpt{
			withLoop(5, []workspace.Predicate{until}, workspace.ExhaustedError),
		}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "step1", Status: delegate.StatusSuccess},
		{Result: "step2", Status: delegate.StatusSuccess},
		{Result: "DONE", Status: delegate.StatusSuccess},
	}
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 3 {
		t.Fatalf("dispatcher calls = %d, want 3", got)
	}
	// Final iteration's content promoted to plain stage path.
	plain := f.session.ArtifactPath("01_loop", "01_loop.md")
	body, err := os.ReadFile(plain)
	if err != nil {
		t.Fatalf("read promoted artifact: %v", err)
	}
	if string(body) != "DONE" {
		t.Errorf("plain artifact body = %q, want DONE", string(body))
	}
}

// 7. Loop exhausts with on_exhausted=error → Run halts.
func TestOrchestrator_LoopExhaustsError(t *testing.T) {
	until := workspace.Predicate{
		Type:    workspace.PredRegex,
		Name:    "looks_done",
		Pattern: "DONE",
	}
	until.SetCompiledRegex(mustRegex("DONE"))
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_loop", opts: []workflowOpt{
			withLoop(2, []workspace.Predicate{until}, workspace.ExhaustedError),
		}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "x", Status: delegate.StatusSuccess},
		{Result: "y", Status: delegate.StatusSuccess},
	}
	if err := f.orch.Run(context.Background()); err == nil {
		t.Fatal("Run: expected error")
	}
}

// 8. Loop exhausts with on_exhausted=human_gate and operator picks
// "restart" → second pass converges.
func TestOrchestrator_LoopExhaustsHumanRestart(t *testing.T) {
	until := workspace.Predicate{
		Type:    workspace.PredRegex,
		Name:    "looks_done",
		Pattern: "DONE",
	}
	until.SetCompiledRegex(mustRegex("DONE"))
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_loop", opts: []workflowOpt{
			withLoop(2, []workspace.Predicate{until}, workspace.ExhaustedHumanGate),
		}},
	})
	f.orch.LoopMaxRestarts = 3
	f.disp.outputs = []delegate.Output{
		{Result: "x", Status: delegate.StatusSuccess},
		{Result: "y", Status: delegate.StatusSuccess},
		{Result: "DONE", Status: delegate.StatusSuccess},
	}
	f.hitl.queue("icm.loop.exhausted", events.HITLResponse{ChoiceID: "restart"})
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 3 {
		t.Fatalf("dispatcher calls = %d, want 3", got)
	}
}

// 9. Fan-out continue policy: 3 items, one fails — run completes and
// aggregate is written.
func TestOrchestrator_FanOutContinue(t *testing.T) {
	// Source stage writes a small JSON list to fan over.
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "00_src", opts: nil},
		{id: "01_fan", opts: []workflowOpt{
			withFanOut("00_src/00_src.md", 2, workspace.ItemFailureContinue),
		}},
	})
	// Inject the source artifact directly so 00_src dispatch finds an
	// array. The source stage only needs to *produce* JSON content.
	sourceBody := `["alpha","beta","gamma"]`
	f.disp.outputs = []delegate.Output{
		{Result: sourceBody, Status: delegate.StatusSuccess},  // 00_src
		{Result: "ok-alpha", Status: delegate.StatusSuccess},  // item 0
		{Result: "", Status: delegate.StatusError, Error: "rate limit"}, // item 1
		{Result: "ok-gamma", Status: delegate.StatusSuccess},  // item 2
	}
	f.disp.errs = []error{nil, nil, errors.New("rate limit"), nil}

	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Aggregate at plain stage path.
	agg := f.session.ArtifactPath("01_fan", "01_fan.md")
	if _, err := os.Stat(agg); err != nil {
		t.Fatalf("aggregate missing: %v", err)
	}
}

// 10. Fan-out halt policy: first failure cancels remaining items, run
// halts.
func TestOrchestrator_FanOutHalt(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "00_src", opts: nil},
		{id: "01_fan", opts: []workflowOpt{
			withFanOut("00_src/00_src.md", 1, workspace.ItemFailureHalt),
		}},
	})
	sourceBody := `["alpha","beta","gamma"]`
	f.disp.outputs = []delegate.Output{
		{Result: sourceBody, Status: delegate.StatusSuccess},
		{Result: "", Status: delegate.StatusError, Error: "fail"},
		{Result: "should-not-run", Status: delegate.StatusSuccess},
		{Result: "should-not-run", Status: delegate.StatusSuccess},
	}
	f.disp.errs = []error{nil, errors.New("fail"), nil, nil}
	if err := f.orch.Run(context.Background()); err == nil {
		t.Fatal("Run: expected halt error")
	}
}

// 11. human_gate=start, choice=allow → stage proceeds.
func TestOrchestrator_GateStartAllow(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withHumanGate(workspace.HumanGateStart)}},
	})
	f.disp.outputs = []delegate.Output{{Result: "OK", Status: delegate.StatusSuccess}}
	f.hitl.queue("icm.stage.start", events.HITLResponse{ChoiceID: "allow"})
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.hitl.calls["icm.stage.start"]; got != 1 {
		t.Errorf("start gate call count = %d, want 1", got)
	}
}

// 12. human_gate=start, choice=reject → run halts before any dispatch.
func TestOrchestrator_GateStartReject(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withHumanGate(workspace.HumanGateStart)}},
	})
	f.hitl.queue("icm.stage.start", events.HITLResponse{ChoiceID: "reject"})
	err := f.orch.Run(context.Background())
	if err == nil {
		t.Fatal("Run: expected reject error")
	}
	if f.disp.callCount() != 0 {
		t.Errorf("dispatcher invoked despite reject: %d calls", f.disp.callCount())
	}
	if got := f.orch.state.Outcome; got != session.OutcomeRejected {
		t.Errorf("Outcome = %q, want %q", got, session.OutcomeRejected)
	}
}

// 13. human_gate=end with restart → stage re-runs once, then accepts.
func TestOrchestrator_GateEndRestart(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withHumanGate(workspace.HumanGateEnd)}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "first", Status: delegate.StatusSuccess},
		{Result: "second", Status: delegate.StatusSuccess},
	}
	f.hitl.queue("icm.stage.end", events.HITLResponse{ChoiceID: "restart"})
	f.hitl.queue("icm.stage.end", events.HITLResponse{ChoiceID: "allow"})
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 2 {
		t.Errorf("dispatcher calls = %d, want 2", got)
	}
}

// 14. Dispatch error with halt policy emits ICMRunHalted on the bus.
func TestOrchestrator_EmitsRunHaltedOnDispatchError(t *testing.T) {
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{withOnError(workspace.ErrorHalt)}},
	})
	f.disp.errs = []error{errors.New("boom")}
	f.disp.outputs = []delegate.Output{{Status: delegate.StatusError, Error: "boom"}}

	var halted icmtypes.ICMRunHalted
	var sawHalt atomic.Bool
	f.bus.Subscribe("icm.run.halted", func(ev engine.Event[any]) {
		if h, ok := ev.Payload.(icmtypes.ICMRunHalted); ok {
			halted = h
			sawHalt.Store(true)
		}
	})
	_ = f.orch.Run(context.Background())
	if !sawHalt.Load() {
		t.Fatal("did not see icm.run.halted")
	}
	if halted.HaltedAtStage != "01_a" {
		t.Errorf("HaltedAtStage = %q, want 01_a", halted.HaltedAtStage)
	}
}

// 15. Happy path emits ICMRunStarted + ICMRunCompleted + per-stage events.
func TestOrchestrator_EmitsLifecycleEvents(t *testing.T) {
	f := newFixture(t, "01_a", "02_b")
	f.disp.outputs = []delegate.Output{
		{Result: "A", Status: delegate.StatusSuccess},
		{Result: "B", Status: delegate.StatusSuccess},
	}
	var (
		mu          sync.Mutex
		started     int
		stageStart  int
		stageDone   int
		completed   int
		turnEvents  int
	)
	f.bus.Subscribe("icm.run.started", func(_ engine.Event[any]) {
		mu.Lock(); started++; mu.Unlock()
	})
	f.bus.Subscribe("icm.run.completed", func(_ engine.Event[any]) {
		mu.Lock(); completed++; mu.Unlock()
	})
	f.bus.Subscribe("icm.stage.started", func(_ engine.Event[any]) {
		mu.Lock(); stageStart++; mu.Unlock()
	})
	f.bus.Subscribe("icm.stage.completed", func(_ engine.Event[any]) {
		mu.Lock(); stageDone++; mu.Unlock()
	})
	f.bus.Subscribe("icm.turn", func(_ engine.Event[any]) {
		mu.Lock(); turnEvents++; mu.Unlock()
	})

	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if started != 1 || completed != 1 {
		t.Errorf("run lifecycle counts: started=%d completed=%d", started, completed)
	}
	if stageStart != 2 || stageDone != 2 {
		t.Errorf("stage lifecycle counts: start=%d done=%d", stageStart, stageDone)
	}
	if turnEvents != 2 {
		t.Errorf("turn event count = %d, want 2", turnEvents)
	}
}

// 16. State file is written + reflects completed outcome.
func TestOrchestrator_StateFileReflectsRun(t *testing.T) {
	f := newFixture(t, "01_a")
	f.disp.outputs = []delegate.Output{{Result: "OK", Status: delegate.StatusSuccess}}
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	statePath := filepath.Join(f.session.RootDir, ".icm", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st session.RunState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if st.Outcome != session.OutcomeCompleted {
		t.Errorf("Outcome = %q, want %q", st.Outcome, session.OutcomeCompleted)
	}
	if len(st.Stages) != 1 || st.Stages[0].Status != session.StageStatusDone {
		t.Errorf("stage state wrong: %+v", st.Stages)
	}
}

// 17. Cancellation: ctx cancel during run → outcome=cancelled.
func TestOrchestrator_Cancellation(t *testing.T) {
	// Single stage; dispatcher sleeps via channel until ctx cancel.
	f := newFixture(t, "01_a")
	released := make(chan struct{})
	// Replace dispatcher with a blocking one.
	f.orch.Runtime = blockingDispatcher{release: released}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- f.orch.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(released)

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("Run: expected cancellation error")
		}
		if got := f.orch.state.Outcome; got != session.OutcomeCancelled {
			t.Errorf("Outcome = %q, want %q", got, session.OutcomeCancelled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// blockingDispatcher blocks until release is closed or ctx is cancelled.
type blockingDispatcher struct{ release chan struct{} }

func (b blockingDispatcher) Run(ctx context.Context, _ delegate.Input) (delegate.Output, error) {
	select {
	case <-ctx.Done():
		return delegate.Output{Status: delegate.StatusCancel, Error: ctx.Err().Error()}, ctx.Err()
	case <-b.release:
		return delegate.Output{Status: delegate.StatusSuccess, Result: "released"}, nil
	}
}

// 18. Turn policy=until_human_approves: continue then allow.
func TestOrchestrator_TurnsUntilHumanApproves_ContinueAllow(t *testing.T) {
	pred := regexPred("must_have_ok", "OK")
	f := newFixtureOpts(t, nil, []stageDef{
		{id: "01_a", opts: []workflowOpt{
			withTurnPolicy(workspace.TurnsUntilHumanApproves, 3),
			withValidator(pred),
		}},
	})
	f.disp.outputs = []delegate.Output{
		{Result: "first try", Status: delegate.StatusSuccess},
		{Result: "second try with OK", Status: delegate.StatusSuccess},
	}
	// First turn fails validator → ask human; respond "continue".
	// Second turn passes validator → no human gate fires.
	f.hitl.queue("icm.stage.turn", events.HITLResponse{ChoiceID: "continue", FreeText: "make it better"})
	if err := f.orch.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.disp.callCount(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", got)
	}
	if got := f.hitl.calls["icm.stage.turn"]; got != 1 {
		t.Errorf("turn gate calls = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func regexPred(name, pattern string) workspace.Predicate {
	p := workspace.Predicate{
		Type:    workspace.PredRegex,
		Name:    name,
		Pattern: pattern,
		Anchor:  workspace.AnchorWhole,
	}
	p.SetCompiledRegex(mustRegex(pattern))
	return p
}

// mustRegex compiles pattern or panics. Used inside workspace.Predicate
// fixtures where compilation errors indicate a test bug, not runtime.
func mustRegex(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}
