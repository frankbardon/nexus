// Package icm implements the nexus.workflows.icm plugin: a file-driven
// multi-stage workflow runner inspired by the Interpretable Context
// Methodology (Van Clief & McDermott, arXiv:2603.16021).
//
// A workspace is a folder on disk. Stages live under stages/NN_slug/,
// each with a contract.md (YAML frontmatter + role body) and an
// optional grounding/ folder. The plugin loads + validates the
// workspace at Init, registers a derived AgentPosture per stage at
// Ready, and dispatches each stage as a sub-agent via a private
// delegate.Runtime when an io.input arrives.
//
// All comms flow through the bus. The plugin owns no LLM client
// directly — every model call routes through delegate (sub-agents +
// LLM judges) and every human interaction through hitl.requested.
package icm

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates/builtins"
	"github.com/frankbardon/nexus/plugins/workflows/icm/runtime"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

//go:embed defaults/operator.md
var defaultOperatorBytes []byte

const (
	defaultPluginID = "nexus.workflows.icm"
	pluginName      = "ICM Workflow Runner"
	pluginVersion   = "0.1.0"

	capPostureRegistry = "posture.registry"
	capWorkflowRunner  = "workflow.runner"
)

// Plugin is the nexus.workflows.icm plugin instance. Multiple instances
// coexist via the instance-suffix mechanism (nexus.workflows.icm/script,
// nexus.workflows.icm/research) — each pinned to its own workspace.
type Plugin struct {
	instanceID string
	bus        engine.EventBus
	logger     *slog.Logger

	// Resolved at Init.
	cfg            Config
	postureID      string // capability provider for posture.registry
	postureReg     posture.Registry
	runtime        *delegate.Runtime
	evaluator      *predicates.Evaluator
	skillToolName  string
	schemas        *engine.SchemaRegistry
	workflow       *workspace.Workflow
	postureBuilder *runtime.PostureBuilder

	// registeredPostures lists names registered with postureReg, in
	// registration order. Shutdown deregisters them in reverse.
	registeredPostures []string
	// registeredSchemas lists names registered with schemas. The
	// SchemaRegistry's engine teardown handles cleanup; tracking is kept
	// for diagnostics and forward-compatibility.
	registeredSchemas []string

	// In-flight invocation tracking (populated by runtime, read by tool
	// handlers like read_skill_reference).
	inflightMu sync.RWMutex
	inflight   map[string]*invocationState // keyed by TurnID

	// HITL wait correlation (capability b).
	hitlMu   sync.Mutex
	hitlWait map[string]chan events.HITLResponse

	// orchestrators tracks active runs keyed by run ID. Populated lazily
	// by buildOrchestrator; the entry-point (handleInput) is responsible
	// for cleaning up entries when a run terminates.
	orchMu        sync.Mutex
	orchestrators map[string]*runtime.Orchestrator

	// dataDir is ctx.DataDir captured at Init for run-tree placement.
	dataDir string

	unsubs []func()
}

// Config is the resolved plugin config (subset of schema.json relevant
// at runtime). Loaded once at Init.
type Config struct {
	Workspace                     string
	DefaultJudgePosture           string
	DefaultWorkflowPosture        string
	CacheSize                     int
	InlineArtifactLimitBytes      int
	LoopMaxRestarts               int
	InputFilename                 string
	TreatInputAsPathIfExists      bool
	WorkspaceInputsDir            string
	AutoIncludeSkillReferenceTool bool
	PredicateCommandTimeoutSecs   int
	EmitProgressThinkingSteps     bool
}

// invocationState is the per-stage, per-turn snapshot kept on the
// in-flight table. The read_skill_reference tool handler reads this to
// correlate a tool.invoke back to the active stage's skill set.
type invocationState struct {
	runID   string
	stageID string
	itemID  string
	// skills is populated lazily by capability g; nil during skeleton.
	skills map[string]any
}

// New returns a default-configured Plugin.
func New() engine.Plugin {
	return &Plugin{
		instanceID: defaultPluginID,
		cfg: Config{
			InlineArtifactLimitBytes:      32 * 1024,
			LoopMaxRestarts:               3,
			InputFilename:                 "input.txt",
			TreatInputAsPathIfExists:      true,
			AutoIncludeSkillReferenceTool: true,
			PredicateCommandTimeoutSecs:   30,
			EmitProgressThinkingSteps:     true,
		},
		inflight:      make(map[string]*invocationState),
		hitlWait:      make(map[string]chan events.HITLResponse),
		orchestrators: make(map[string]*runtime.Orchestrator),
	}
}

// ID returns the configured plugin ID (with instance suffix when set).
func (p *Plugin) ID() string { return p.instanceID }

// Name returns the human-readable name.
func (p *Plugin) Name() string { return pluginName }

// Version returns the plugin version string.
func (p *Plugin) Version() string { return pluginVersion }

// Dependencies declares ordering-only deps. None: ICM resolves all
// required capabilities through Requires().
func (p *Plugin) Dependencies() []string { return nil }

// Requires declares the capabilities ICM must have active.
func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{Capability: capPostureRegistry, Optional: false},
	}
}

// Capabilities advertises workflow.runner so other plugins can detect
// the presence of a workflow runner without string-matching plugin IDs.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{Name: capWorkflowRunner, Description: "File-driven multi-stage workflow runner (ICM)."},
	}
}

// Subscriptions lists the events ICM consumes.
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 50},
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "hitl.responded", Priority: 50},
	}
}

// Emissions lists the events ICM may emit. Read by contract tests.
func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"tool.result",
		"before:tool.result",
		"io.output",
		"io.output.stream",
		"io.status",
		"thinking.step",
		"plan.created",
		"plan.progress",
		"hitl.requested",
		"before:hitl.requested",
		"hitl.cancel",
		"delegate.start",
		"delegate.complete",
		"llm.request",
		"before:llm.request",
		"schema.register",
		"icm.run.started",
		"icm.run.completed",
		"icm.run.halted",
		"icm.stage.started",
		"icm.stage.completed",
		"icm.stage.failed",
		"icm.stage.iteration",
		"icm.turn",
		"icm.fanout.item",
		"icm.predicate.failed",
	}
}

// Init parses config, resolves the posture registry, builds the
// private delegate runtime, and subscribes to bus events. Workspace
// loading + posture registration happen in Ready (after every other
// plugin has initialized).
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	if ctx.InstanceID != "" {
		p.instanceID = ctx.InstanceID
	}
	p.skillToolName = runtime.SkillToolName(p.instanceID)
	p.schemas = ctx.Schemas
	p.dataDir = ctx.DataDir

	if err := p.parseConfig(ctx.Config); err != nil {
		return err
	}

	// Resolve posture.registry capability.
	if providers := ctx.Capabilities[capPostureRegistry]; len(providers) > 0 {
		p.postureID = providers[0]
	}
	if p.postureID == "" {
		return fmt.Errorf("icm: no provider for capability %s", capPostureRegistry)
	}
	regPlugin := ctx.LookupPlugin(p.postureID)
	if regPlugin == nil {
		return fmt.Errorf("icm: plugin %q not loaded", p.postureID)
	}
	type registryProvider interface {
		Registry() posture.Registry
	}
	rp, ok := regPlugin.(registryProvider)
	if !ok {
		return fmt.Errorf("icm: plugin %q does not expose Registry()", p.postureID)
	}
	p.postureReg = rp.Registry()

	// Build the private delegate runtime. Cache disabled by default
	// (capability a); operators opt in via cache_size > 0.
	p.runtime = &delegate.Runtime{
		Registry: p.postureReg,
		Bus:      p.bus,
		Logger:   p.logger,
		MaxDepth: 3,
	}
	if p.cfg.CacheSize > 0 {
		p.runtime.Cache = delegate.NewMemoryCache(p.cfg.CacheSize)
	}

	// Predicate evaluator + baked-in native handlers. Judge/Human
	// dispatchers stay nil until later wiring steps; the four built-in
	// native handlers are usable today.
	p.evaluator = predicates.NewEvaluator(ctx.Schemas, ctx.Sandbox, p.bus, p.logger)
	p.evaluator.CommandTimeoutSecs = p.cfg.PredicateCommandTimeoutSecs
	if err := builtins.RegisterAll(p.evaluator); err != nil {
		return fmt.Errorf("icm: register builtin native predicates: %w", err)
	}
	p.installPredicateDispatchers()

	// Workspace load: the on-disk contract is parsed + validated up front
	// so subsequent Ready()-time posture registration can fail fast on
	// shape errors. Embedded operator.md serves as the fallback when the
	// workspace omits its own.
	wf, err := workspace.LoadWorkspace(p.cfg.Workspace,
		workspace.WithDefaultOperatorBytes(defaultOperatorBytes))
	if err != nil {
		return fmt.Errorf("icm: workspace load: %w", err)
	}
	p.workflow = wf

	p.postureBuilder = &runtime.PostureBuilder{
		Workflow:             wf,
		InstanceID:           p.instanceID,
		DefaultBasePosture:   p.cfg.DefaultWorkflowPosture,
		Registry:             p.postureReg,
		SkillToolName:        p.skillToolName,
		AutoIncludeSkillTool: p.cfg.AutoIncludeSkillReferenceTool,
	}

	// Bus subscriptions. Per-event handlers are stubs in the skeleton —
	// later steps wire them.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput,
			engine.WithPriority(50), engine.WithSource(p.instanceID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(50), engine.WithSource(p.instanceID)),
		p.bus.Subscribe("hitl.responded", p.handleHITLResponded,
			engine.WithPriority(50), engine.WithSource(p.instanceID)),
	)

	return nil
}

// Ready registers per-stage output schemas, derives + registers a posture
// per stage and verifier, then registers the LLM-facing icm_validate
// tool. Halts boot on any failure so an invalid workspace contract never
// reaches dispatch.
func (p *Plugin) Ready() error {
	if err := p.registerJudgeResponseSchema(); err != nil {
		return err
	}
	if err := p.registerSchemas(); err != nil {
		return err
	}
	if err := p.registerPostures(); err != nil {
		return err
	}
	if err := p.registerValidateTool(); err != nil {
		return err
	}
	return nil
}

// Shutdown unsubscribes from the bus and deregisters any postures the
// plugin owns. Schemas are not deregistered here — the engine teardown
// drops the SchemaRegistry wholesale, and there is no per-source removal
// in the public surface.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil

	// Reverse-order deregistration mirrors construction so any layered
	// caches keyed by posture name see a coherent teardown.
	for i := len(p.registeredPostures) - 1; i >= 0; i-- {
		name := p.registeredPostures[i]
		if err := p.postureReg.Remove(name); err != nil && p.logger != nil {
			p.logger.Warn("icm: deregister posture", "name", name, "err", err)
		}
	}
	p.registeredPostures = nil
	return nil
}

// handleInput is the io.input entry point. Each io.input begins a new
// ICM run: a fresh runID + session dir, optional initial-input copy,
// plan.created emission, orchestrator construction, and Run dispatched
// in its own goroutine so the bus is not blocked across the
// (potentially long) workflow.
func (p *Plugin) handleInput(ev engine.Event[any]) {
	in, ok := ev.Payload.(events.UserInput)
	if !ok {
		return
	}
	if p.workflow == nil {
		p.logger.Warn("icm: io.input received but workflow not loaded; ignoring")
		return
	}
	go p.runWorkflow(in)
}

// handleToolInvoke dispatches read_skill_reference + icm_validate. Skeleton
// only handles validate; skills tool wires in capability (g).
func (p *Plugin) handleToolInvoke(ev engine.Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok {
		return
	}
	switch tc.Name {
	case p.validateToolName():
		p.handleValidateInvoke(tc)
	}
}

// handleHITLResponded delivers a HITLResponse to any in-flight ICM
// waiter keyed by request ID. Skeleton handler is safe to call from
// step 0 onwards; further steps populate p.hitlWait.
func (p *Plugin) handleHITLResponded(ev engine.Event[any]) {
	resp, ok := ev.Payload.(events.HITLResponse)
	if !ok {
		return
	}
	if !strings.HasPrefix(resp.RequestID, "icm-") {
		return
	}
	p.hitlMu.Lock()
	ch := p.hitlWait[resp.RequestID]
	p.hitlMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// parseConfig coerces map[string]any (from YAML) into the typed Config.
// Validates required fields and applies defaults.
func (p *Plugin) parseConfig(raw map[string]any) error {
	if ws, ok := raw["workspace"].(string); ok && ws != "" {
		p.cfg.Workspace = engine.ExpandPath(ws)
	}
	if p.cfg.Workspace == "" {
		return errors.New("icm: workspace is required")
	}
	if v, ok := raw["default_judge_posture"].(string); ok {
		p.cfg.DefaultJudgePosture = v
	}
	if v, ok := raw["default_workflow_posture"].(string); ok {
		p.cfg.DefaultWorkflowPosture = v
	}
	if v, ok := intConfig(raw, "cache_size"); ok {
		p.cfg.CacheSize = v
	}
	if v, ok := intConfig(raw, "inline_artifact_limit_bytes"); ok {
		p.cfg.InlineArtifactLimitBytes = v
	}
	if v, ok := intConfig(raw, "loop_max_restarts"); ok {
		p.cfg.LoopMaxRestarts = v
	}
	if v, ok := raw["input_filename"].(string); ok && v != "" {
		p.cfg.InputFilename = v
	}
	if v, ok := raw["treat_input_as_path_if_exists"].(bool); ok {
		p.cfg.TreatInputAsPathIfExists = v
	}
	if v, ok := raw["workspace_inputs_dir"].(string); ok && v != "" {
		p.cfg.WorkspaceInputsDir = engine.ExpandPath(v)
	}
	if v, ok := raw["auto_include_skill_reference_tool"].(bool); ok {
		p.cfg.AutoIncludeSkillReferenceTool = v
	}
	if v, ok := intConfig(raw, "predicate_command_timeout_seconds"); ok {
		p.cfg.PredicateCommandTimeoutSecs = v
	}
	if v, ok := raw["emit_progress_thinking_steps"].(bool); ok {
		p.cfg.EmitProgressThinkingSteps = v
	}
	return nil
}

// intConfig coerces a YAML numeric (which yaml.v3 lands as int or
// float64 depending on shape) into an int.
func intConfig(raw map[string]any, key string) (int, bool) {
	v, present := raw[key]
	if !present {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	}
	return 0, false
}

// validateToolName returns the instance-scoped name for icm_validate.
func (p *Plugin) validateToolName() string {
	if i := strings.LastIndexByte(p.instanceID, '/'); i >= 0 && i+1 < len(p.instanceID) {
		return "icm_validate_" + p.instanceID[i+1:]
	}
	return "icm_validate"
}

// registerValidateTool registers the LLM-facing workspace validator
// tool. Full implementation in step 12; skeleton registers the def so
// the LLM sees it and the contract test can confirm registration.
func (p *Plugin) registerValidateTool() error {
	def := events.ToolDef{
		Name:        p.validateToolName(),
		Description: "Validate an ICM workspace folder. Returns aggregated load errors (one per line) when validation fails, or 'ok' when the workspace loads cleanly.",
		Class:       "workflow",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace": map[string]any{
					"type":        "string",
					"description": "Path to the workspace folder to validate. Defaults to the plugin's configured workspace when omitted.",
				},
			},
		},
	}
	return p.bus.Emit("tool.register", def)
}

// handleValidateInvoke runs the workspace loader against the supplied
// path (or the plugin's configured workspace when absent) and returns
// the aggregated load errors as the tool output. "ok" on success.
func (p *Plugin) handleValidateInvoke(tc events.ToolCall) {
	target, _ := tc.Arguments["workspace"].(string)
	if target == "" {
		target = p.cfg.Workspace
	} else {
		target = engine.ExpandPath(target)
	}

	output := "ok"
	if err := workspace.Validate(target, workspace.WithDefaultOperatorBytes(defaultOperatorBytes)); err != nil {
		output = formatLoadErrors(err)
	}

	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        output,
		TurnID:        tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &res); err == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("tool.result", res)
}

// formatLoadErrors renders a workspace.LoadErrors as a multi-line
// summary suitable for human + LLM consumption. Single-error cases
// collapse onto one line.
func formatLoadErrors(err error) string {
	if err == nil {
		return "ok"
	}
	// LoadErrors.Error() already renders nicely; pass through.
	return err.Error()
}

// registerSchemas reads + registers JSON schemas referenced by stages
// and verifiers. The plugin owns three classes of registration:
//
//  1. Stage output schemas (output.format=json, output.schema set) under
//     "icm.[<suffix>.]<stageID>.output".
//  2. Validator schemas (output.validators with type=schema) under
//     "icm.[<suffix>.]<stageID>.<predicateName>".
//  3. Loop exit-condition schemas (loop.until with type=schema) under the
//     same predicate-scoped key.
//
// All schema paths are resolved relative to the workspace root and read
// from disk. Read or parse failures halt boot.
func (p *Plugin) registerSchemas() error {
	if p.workflow == nil || p.schemas == nil {
		return nil
	}
	for i := range p.workflow.Stages {
		stage := &p.workflow.Stages[i]
		if err := p.registerStageSchemas(stage); err != nil {
			return err
		}
	}
	for _, verifier := range p.workflow.Verifiers {
		if err := p.registerStageSchemas(verifier); err != nil {
			return err
		}
	}
	return nil
}

// registerStageSchemas handles a single stage's output + predicate
// schemas. Verifiers reuse the same code because they share the Stage
// type.
func (p *Plugin) registerStageSchemas(stage *workspace.Stage) error {
	if stage.Output.Format == workspace.OutputJSON && stage.Output.Schema != "" {
		name := p.postureBuilder.SchemaName(stage.ID)
		if err := p.registerSchemaFile(name, stage.Output.Schema); err != nil {
			return fmt.Errorf("icm: stage %q output schema: %w", stage.ID, err)
		}
	}
	for i := range stage.Output.Validators {
		pred := &stage.Output.Validators[i]
		if pred.Type != workspace.PredSchema || pred.SchemaPath == "" {
			continue
		}
		predName := pred.Name
		if predName == "" {
			predName = fmt.Sprintf("validator_%d", i)
		}
		name := p.postureBuilder.PredicateSchemaName(stage.ID, predName)
		if err := p.registerSchemaFile(name, pred.SchemaPath); err != nil {
			return fmt.Errorf("icm: stage %q validator %q: %w", stage.ID, predName, err)
		}
	}
	if stage.Loop != nil {
		for i := range stage.Loop.Until {
			pred := &stage.Loop.Until[i]
			if pred.Type != workspace.PredSchema || pred.SchemaPath == "" {
				continue
			}
			predName := pred.Name
			if predName == "" {
				predName = fmt.Sprintf("until_%d", i)
			}
			name := p.postureBuilder.PredicateSchemaName(stage.ID, predName)
			if err := p.registerSchemaFile(name, pred.SchemaPath); err != nil {
				return fmt.Errorf("icm: stage %q loop.until %q: %w", stage.ID, predName, err)
			}
		}
	}
	return nil
}

// registerSchemaFile reads a JSON schema from a workspace-relative path
// and installs it under name in the engine schema registry.
func (p *Plugin) registerSchemaFile(name, relPath string) error {
	abs := relPath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(p.workflow.Root, relPath)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read %s: %w", relPath, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return fmt.Errorf("parse %s: %w", relPath, err)
	}
	p.schemas.Register(name, schema, "icm")
	p.registeredSchemas = append(p.registeredSchemas, name)
	return nil
}

// buildOrchestrator constructs a runtime.Orchestrator wired against the
// plugin's resolved state. The session is supplied by the caller (the
// io.input entry point creates it once per run). The orchestrator is
// tracked on p.orchestrators so the run can be looked up by ID.
func (p *Plugin) buildOrchestrator(runID string, sess *session.Session) *runtime.Orchestrator {
	payload := &runtime.PayloadBuilder{
		Workflow:                 p.workflow,
		Session:                  sess,
		InlineArtifactLimitBytes: p.cfg.InlineArtifactLimitBytes,
		Logger:                   p.logger,
	}
	o := &runtime.Orchestrator{
		Workflow:          p.workflow,
		Session:           sess,
		Runtime:           p.runtime,
		Evaluator:         p.evaluator,
		Payload:           payload,
		PostureBuilder:    p.postureBuilder,
		Bus:               p.bus,
		Logger:            p.logger,
		HITLDispatch:      p.emitHITLAndWait,
		NewHITLID:         newHITLID,
		InstanceID:        p.instanceID,
		RunID:             runID,
		LoopMaxRestarts:   p.cfg.LoopMaxRestarts,
		EmitThinkingSteps: p.cfg.EmitProgressThinkingSteps,
	}
	p.orchMu.Lock()
	p.orchestrators[runID] = o
	p.orchMu.Unlock()
	return o
}

// registerPostures derives + installs an AgentPosture per stage and
// verifier. Names are tracked so Shutdown can deregister them.
func (p *Plugin) registerPostures() error {
	if p.workflow == nil || p.postureBuilder == nil {
		return nil
	}
	for i := range p.workflow.Stages {
		stage := &p.workflow.Stages[i]
		ap, err := p.postureBuilder.Build(stage)
		if err != nil {
			return fmt.Errorf("icm: derive posture for stage %q: %w", stage.ID, err)
		}
		if err := p.postureReg.Register(ap); err != nil {
			return fmt.Errorf("icm: register posture %q: %w", ap.Name, err)
		}
		p.registeredPostures = append(p.registeredPostures, ap.Name)
	}
	for id, verifier := range p.workflow.Verifiers {
		ap, err := p.postureBuilder.BuildVerifier(verifier)
		if err != nil {
			return fmt.Errorf("icm: derive posture for verifier %q: %w", id, err)
		}
		if err := p.postureReg.Register(ap); err != nil {
			return fmt.Errorf("icm: register posture %q: %w", ap.Name, err)
		}
		p.registeredPostures = append(p.registeredPostures, ap.Name)
	}
	return nil
}
