// Package workspace is the pure-Go loader and validator for an ICM
// workspace folder. It has no Nexus runtime dependencies; given an
// absolute path it returns a fully validated *Workflow value or a
// *LoadErrors aggregate. All field references, regex patterns, JSON
// schemas, and gojq expressions are resolved at load time so the
// orchestrator can dispatch without re-parsing.
//
// Callers (the icm plugin) are responsible for tilde-expansion via
// engine.ExpandPath before invoking the loader; this package only sees
// absolute paths.
package workspace

import (
	"regexp"
	"strconv"

	"github.com/itchyny/gojq"
)

// Workflow is the in-memory representation of a loaded workspace.
// Produced by LoadWorkspace; consumed by the orchestrator.
type Workflow struct {
	// Root is the absolute path of the workspace root.
	Root string
	// LayerNames are the resolved file/folder name overrides.
	LayerNames LayerNames
	// Operator is the resolved operator system-prompt template.
	Operator OperatorConfig
	// Defaults are the workspace-level defaults from icm.yaml.
	Defaults WorkspaceDefaults
	// WorkspaceDoc is the raw body of workspace.md.
	WorkspaceDoc string
	// Stages are in execution order (sorted by numeric folder prefix).
	Stages []Stage
	// Verifiers are keyed by ID.
	Verifiers map[string]*Stage
}

// LayerNames maps the conceptual workspace layers to their on-disk
// filenames. Defaults are baked into the loader; icm.yaml overrides any
// subset.
type LayerNames struct {
	// Operator is the operator system-prompt filename. Default "operator.md".
	Operator string `yaml:"operator"`
	// Workspace is the workspace doc filename. Default "workspace.md".
	Workspace string `yaml:"workspace"`
	// Contract is the per-stage contract filename. Default "contract.md".
	Contract string `yaml:"contract"`
	// Grounding is the per-stage grounding folder name. Default "grounding".
	Grounding string `yaml:"grounding"`
}

// OperatorConfig is the resolved operator system-prompt template. Body
// is raw template text; the orchestrator renders it through text/template
// at sub-agent spawn time.
type OperatorConfig struct {
	// Body is the rendered template source (workspace body + overlay if any).
	Body string
	// Source tells the operator where Body originated.
	// One of: "workspace", "default", "default+overlay", "workspace+overlay".
	Source string
	// Overlay is the appended overlay text, if any.
	Overlay string
}

// WorkspaceDefaults are the workspace-level defaults from icm.yaml.
// Stage-level fields override these; absent fields fall back to library
// defaults at dispatch.
type WorkspaceDefaults struct {
	// TurnPolicy is the default per-stage turn policy.
	TurnPolicy TurnPolicy `yaml:"turn_policy"`
	// HumanGate is the default per-stage human-gate position.
	HumanGate HumanGate `yaml:"human_gate"`
	// OnError is the default per-stage error policy.
	OnError ErrorPolicy `yaml:"on_error"`
	// JudgePosture is the posture name used for `type: llm` predicates
	// when the predicate does not name an explicit `model:` posture.
	JudgePosture string `yaml:"judge_posture"`
	// Agent is the default agent spec applied when a stage omits one.
	Agent AgentSpec `yaml:"agent"`
}

// Stage is a single step in the workflow. Each stage runs as a sub-agent
// with a scoped context (operator prompt + this stage's grounding +
// declared prior artifacts).
type Stage struct {
	// ID is derived from the folder name (e.g. "02_script") and may be
	// echoed in front-matter id.
	ID string
	// Display is the optional human-readable label. Derived from
	// front-matter display, the contract body's first non-empty line, or
	// (last-ditch) ID.
	Display string
	// Folder is the absolute path to the stage folder.
	Folder string
	// Role is the contract body (after frontmatter), rendered into the
	// operator prompt at dispatch.
	Role string
	// Turns governs inner-loop behavior within a single stage invocation.
	Turns TurnConfig
	// HumanGate declares where human review fires at the stage boundary.
	HumanGate HumanGate
	// OnError governs non-validator stage failure handling.
	OnError ErrorPolicy
	// Loop is nil for non-looping stages.
	Loop *LoopConfig
	// FanOut is nil for non-fan-out stages.
	FanOut *FanOutConfig
	// Output declares what the stage writes and how it's validated.
	Output OutputSpec
	// Inputs declares what files the stage reads.
	Inputs InputScope
	// Agent configures the sub-agent (model role, tools, posture, budget).
	Agent AgentSpec
	// Verifiers are verifier IDs declared for this stage.
	Verifiers []string
	// Skills are resolved skill bundles keyed by name. Stage-local can
	// shadow workspace-shared.
	Skills map[string]*Skill
}

// TurnPolicy enumerates how the inner-turn loop terminates.
type TurnPolicy string

const (
	// TurnsFixed runs Turns.Max turns unconditionally (default).
	TurnsFixed TurnPolicy = "fixed"
	// TurnsUntilValid retries up to Turns.Max times while validators fail.
	TurnsUntilValid TurnPolicy = "until_valid"
	// TurnsUntilHumanApproves loops while the human reviewer rejects.
	TurnsUntilHumanApproves TurnPolicy = "until_human_approves"
)

// TurnConfig governs inner-loop behavior within a single stage
// invocation. See LoopConfig for cross-invocation iteration.
type TurnConfig struct {
	Policy TurnPolicy `yaml:"policy"`
	// Max defaults to 1 for Fixed and 3 for UntilValid.
	Max int `yaml:"max"`
}

// HumanGate declares where human review fires at the stage boundary. On
// looping stages this fires at the bounds of the entire stage, not per
// iteration; use a Human predicate in Loop.Until for per-iteration review.
type HumanGate string

const (
	HumanGateNone  HumanGate = "none"
	HumanGateStart HumanGate = "start"
	HumanGateEnd   HumanGate = "end"
	HumanGateBoth  HumanGate = "both"
)

// ErrorPolicy governs how the orchestrator handles non-validator stage
// failures: LLM API errors, malformed output that can't be parsed for
// schema validation, tool call failures, fan-out source not resolving to
// an array.
type ErrorPolicy string

const (
	// ErrorHalt stops the run (default).
	ErrorHalt ErrorPolicy = "halt"
	// ErrorRetry retries the current turn.
	ErrorRetry ErrorPolicy = "retry"
	// ErrorHumanGate pauses for a human decision.
	ErrorHumanGate ErrorPolicy = "human_gate"
)

// LoopConfig declares that the entire stage iterates until the Until
// predicates all pass, or MaxIterations is reached.
type LoopConfig struct {
	MaxIterations int             `yaml:"max_iterations"`
	Until         []Predicate     `yaml:"until"`
	OnExhausted   ExhaustedAction `yaml:"on_exhausted"`
}

// ExhaustedAction governs behaviour when MaxIterations runs out.
type ExhaustedAction string

const (
	// ExhaustedHumanGate raises a HITL gate (default).
	ExhaustedHumanGate ExhaustedAction = "human_gate"
	// ExhaustedError treats convergence failure as a stage error.
	ExhaustedError ExhaustedAction = "error"
)

// FanOutConfig declares the stage runs once per item in a list,
// dispatching a fresh sub-agent per item. Distinct from LoopConfig: loop
// is convergence-driven, fan-out is data-driven. They compose — a stage
// with both fans out per item, and each item independently iterates.
type FanOutConfig struct {
	// Source is an artifact reference "<stage_id>/<filename>" resolved
	// against the active session at dispatch; must point at JSON.
	Source string `yaml:"source"`
	// JSONPath optionally navigates into the source document to locate
	// the array. Defaults to "." (whole document) when empty.
	JSONPath string `yaml:"jsonpath"`
	// ItemVar is the variable name the per-invocation item receives in
	// the XML payload.
	ItemVar string `yaml:"item_var"`
	// ItemID is an optional gojq expression evaluated against each item
	// to name the per-item folder. Falls back to "item_NN".
	ItemID string `yaml:"item_id"`
	// MaxParallel caps concurrent item invocations. Defaults to 1.
	MaxParallel int `yaml:"max_parallel"`
	// OnItemFailure controls what happens when an item fails after
	// exhausting its turn/loop budgets.
	OnItemFailure ItemFailureAction `yaml:"on_item_failure"`

	// compiledJSONPath is the gojq-compiled code for JSONPath; nil when
	// JSONPath is empty.
	compiledJSONPath *gojq.Code
	// compiledItemID is the gojq-compiled code for ItemID; nil when
	// ItemID is empty.
	compiledItemID *gojq.Code
}

// CompiledJSONPath returns the pre-compiled gojq program for JSONPath.
// Nil when JSONPath is empty (meaning "the whole source document").
func (f *FanOutConfig) CompiledJSONPath() *gojq.Code { return f.compiledJSONPath }

// CompiledItemID returns the pre-compiled gojq program for ItemID.
// Nil when no item_id was declared.
func (f *FanOutConfig) CompiledItemID() *gojq.Code { return f.compiledItemID }

// ItemFailureAction enumerates fan-out per-item failure handling.
type ItemFailureAction string

const (
	// ItemFailureContinue skips the failed item and continues (default).
	ItemFailureContinue ItemFailureAction = "continue"
	// ItemFailureHalt aborts the whole fan-out.
	ItemFailureHalt ItemFailureAction = "halt"
)

// OutputSpec defines what the stage writes and how it's validated.
type OutputSpec struct {
	Format     OutputFormat `yaml:"format"`
	Schema     string       `yaml:"schema"`
	Persist    PersistMode  `yaml:"persist"`
	Filename   string       `yaml:"filename"`
	Validators []Predicate  `yaml:"validators"`
}

// OutputFormat enumerates artifact serialization formats.
type OutputFormat string

const (
	OutputText OutputFormat = "text"
	OutputJSON OutputFormat = "json"
)

// PersistMode controls where the stage output is preserved between stages.
type PersistMode string

const (
	PersistContext PersistMode = "context"
	PersistFileRef PersistMode = "file_ref"
	PersistBoth    PersistMode = "both"
)

// Predicate is the unified shape for output validators and loop exit
// conditions. The Type field discriminates; only the relevant per-type
// fields are populated. The loader validates that required fields for
// the declared type are present and compiles regex/jq/schema at load.
type Predicate struct {
	Type PredicateType `yaml:"type"`

	// Name disambiguates multiple predicates of the same type in failure
	// feedback. Defaults to "<type>_<index>".
	Name string `yaml:"name,omitempty"`

	// SchemaPath is the JSON-schema file path (valid for type=schema).
	SchemaPath string `yaml:"schema,omitempty"`

	// Regex predicate fields.
	Pattern string      `yaml:"pattern,omitempty"`
	Anchor  RegexAnchor `yaml:"anchor,omitempty"`
	Message string      `yaml:"message,omitempty"`

	// LLM predicate fields.
	Rubric string `yaml:"rubric,omitempty"`
	// Model is reinterpreted as a posture name (not raw model name) in
	// Nexus' role-based model registry world.
	Model string `yaml:"model,omitempty"`

	// Command predicate fields.
	Run string `yaml:"run,omitempty"`
	// TimeoutSeconds overrides the plugin's default command timeout.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`

	// Native predicate fields.
	Handler string         `yaml:"handler,omitempty"`
	Args    map[string]any `yaml:"args,omitempty"`

	// Human predicate fields.
	Prompt                    string `yaml:"prompt,omitempty"`
	RequireFeedbackOnContinue *bool  `yaml:"require_feedback_on_continue,omitempty"`

	// compiledRegex is populated for regex predicates so the orchestrator
	// does not recompile at evaluation time.
	compiledRegex *regexp.Regexp
}

// CompiledRegex returns the pre-compiled pattern for a regex predicate.
// Nil for non-regex predicates or when the pattern failed to compile.
func (p *Predicate) CompiledRegex() *regexp.Regexp { return p.compiledRegex }

// SetCompiledRegex installs a pre-compiled regex on the predicate. The
// loader calls this internally; callers synthesizing predicates outside
// the loader (e.g. tests, or future stage-output predicate
// synthesizers) use it to wire the regex without re-exporting the
// field.
func (p *Predicate) SetCompiledRegex(re *regexp.Regexp) { p.compiledRegex = re }

// PredicateType enumerates the six predicate kinds.
type PredicateType string

const (
	PredSchema  PredicateType = "schema"
	PredRegex   PredicateType = "regex"
	PredNative  PredicateType = "native"
	PredCommand PredicateType = "command"
	PredLLM     PredicateType = "llm"
	PredHuman   PredicateType = "human"
)

// RegexAnchor scopes regex evaluation within the candidate body.
type RegexAnchor string

const (
	AnchorFirstLine RegexAnchor = "first_line"
	AnchorLastLine  RegexAnchor = "last_line"
	AnchorWhole     RegexAnchor = "whole"
)

// InputScope declares what files the stage reads. Existence is verified
// at load time; missing files cause load errors.
type InputScope struct {
	// Grounding paths are relative to the stage's grounding/ folder.
	Grounding []string `yaml:"grounding"`
	// SharedGrounding paths are relative to shared/grounding/.
	SharedGrounding []string `yaml:"shared_grounding"`
	// Artifacts are logical refs "<stage_id>/<filename>".
	Artifacts []string `yaml:"artifacts"`
	// Skills are skill names; resolved through the precedence chain.
	Skills []string `yaml:"skills"`
}

// Skill is a resolved skill bundle: a folder with a SKILL.md and an
// optional references/ subfolder. SKILL.md loads inline into the agent's
// context; references load on demand via a tool.
type Skill struct {
	// Name is from SKILL.md frontmatter; must match folder name.
	Name string
	// Description is from SKILL.md frontmatter.
	Description string
	// Source identifies which discovery layer found this skill.
	Source SkillSource
	// Path is the absolute path to the skill folder.
	Path string
	// Body is the SKILL.md content after frontmatter.
	Body string
	// References enumerates files under references/, available on demand.
	References []SkillRef
}

// SkillSource enumerates the skill discovery layers in precedence order.
type SkillSource string

const (
	// SkillStageLocal: stages/NN_slug/skills/<name>/
	SkillStageLocal SkillSource = "stage"
	// SkillWorkspace: shared/skills/<name>/
	SkillWorkspace SkillSource = "workspace"
)

// SkillRef is one entry under a skill's references/ folder.
type SkillRef struct {
	// Path is relative to the skill's references/ folder.
	Path string
	// Description is optional, sourced from a leading frontmatter or comment.
	Description string
}

// AgentSpec configures the sub-agent that runs this stage. All fields
// are optional; absent fields fall back to workspace defaults, then to
// library defaults at dispatch.
type AgentSpec struct {
	// Posture is an optional registered-posture name. The loader
	// validates shape only; existence is checked at runtime.
	Posture string `yaml:"posture,omitempty"`
	// ModelRole maps to the engine model registry (not a raw model name).
	ModelRole string `yaml:"model_role,omitempty"`
	// Tools are tool names resolved against the catalog at dispatch.
	Tools []string `yaml:"tools,omitempty"`
	// PromptOverlay is appended to the base operator prompt for this stage.
	PromptOverlay string `yaml:"prompt_overlay,omitempty"`
	// Budget bounds runtime resource use; mirrors posture.ResourceBudget.
	Budget AgentBudget `yaml:"budget,omitempty"`
	// MaxRecursionDepth caps sub-agent nesting (0 = registry default).
	MaxRecursionDepth int `yaml:"max_recursion_depth,omitempty"`
}

// AgentBudget mirrors posture.ResourceBudget without importing the
// runtime package. Zero values mean "use default".
type AgentBudget struct {
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	MaxTokens      int `yaml:"max_tokens,omitempty"`
	MaxToolCalls   int `yaml:"max_tool_calls,omitempty"`
}

// LoadError carries a single workspace validation failure. Loader
// collects all errors before returning so the user can fix the workspace
// in one pass.
type LoadError struct {
	// Path is the file the error originates from.
	Path string
	// Line is 0 when not applicable.
	Line int
	// Stage is the stage ID when applicable, "" otherwise.
	Stage string
	// Msg is the human-readable description.
	Msg string
	// Cause is the underlying error, if any.
	Cause error
}

// Error renders the LoadError as "<path>:<line>: <msg>".
func (e *LoadError) Error() string {
	if e.Line > 0 {
		return e.Path + ":" + strconv.Itoa(e.Line) + ": " + e.Msg
	}
	if e.Path != "" {
		return e.Path + ": " + e.Msg
	}
	return e.Msg
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *LoadError) Unwrap() error { return e.Cause }

// LoadErrors aggregates multiple LoadError values from a single load.
type LoadErrors struct {
	Errors []*LoadError
}

// Error renders the aggregate. Single-error aggregates render as the
// inner error; multi-error aggregates render as a header + bullet list.
func (es *LoadErrors) Error() string {
	if len(es.Errors) == 1 {
		return es.Errors[0].Error()
	}
	s := "workspace load failed (" + strconv.Itoa(len(es.Errors)) + " errors):\n"
	for _, e := range es.Errors {
		s += "  - " + e.Error() + "\n"
	}
	return s
}
