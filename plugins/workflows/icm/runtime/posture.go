// Package runtime — posture derivation for ICM stage dispatch.
//
// This file builds a `posture.AgentPosture` for each stage and verifier in
// the workspace by layering:
//
//  1. The plugin's `default_workflow_posture` (registry lookup) — provides
//     base Model/AllowedTools/Budget/MaxRecursionDepth.
//  2. The stage's named `agent.posture` from the registry (when set,
//     replaces the layer-1 base entirely).
//  3. The stage's `agent.{model_role, tools, prompt_overlay, budget,
//     max_recursion_depth}` overrides applied on top.
//
// The base posture's SystemPrompt is always discarded. The derived
// SystemPrompt is rendered fresh from `workspace.Operator.Body` (workspace
// operator template, already including the workspace operator overlay when
// present) and the stage-level `agent.prompt_overlay`. Run-time values
// (runID, turn, iteration, item) never enter the system prompt — they live
// only on the per-turn XML payload.
package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// OperatorTemplateCtx is the value passed to the workspace operator
// template at posture build time. The Run vars deliberately do NOT appear:
// dynamic per-turn context lives only in the XML payload's <icm_turn>
// attributes, never in the system prompt.
type OperatorTemplateCtx struct {
	// Workspace describes the workflow as a whole.
	Workspace WorkspaceTplCtx
	// Stage describes the single stage this posture dispatches.
	Stage StageTplCtx
}

// WorkspaceTplCtx is the workspace-level slice of OperatorTemplateCtx.
type WorkspaceTplCtx struct {
	// Name is the basename of the workspace root folder.
	Name string
	// Description is the first non-empty line of workspace.md.
	Description string
}

// StageTplCtx is the stage-level slice of OperatorTemplateCtx.
type StageTplCtx struct {
	// ID is the stage folder ID (e.g. "02_script") or verifier ID.
	ID string
	// Display is the human-readable label resolved by the loader.
	Display string
	// Role is the contract body (markdown after frontmatter).
	Role string
	// Output is a flattened view of the stage's output spec.
	Output OutputTplCtx
	// HumanGate is the stage's human-gate position rendered as a string.
	HumanGate string
}

// OutputTplCtx is the stage output slice exposed to the operator template.
type OutputTplCtx struct {
	// Format is "text" or "json".
	Format string
	// Filename is the artifact filename.
	Filename string
	// Schema is the workspace-relative JSON schema path; empty for text
	// stages. Surfaced so the prompt can reference it for documentation.
	Schema string
}

// PostureBuilder derives a posture per stage by composing the plugin's
// default workflow posture, an optional stage-named base posture, and the
// stage's per-field overrides.
//
// One builder is constructed per plugin instance at Ready() time. It is
// stateless beyond its configuration and safe for sequential use.
type PostureBuilder struct {
	// Workflow is the loaded workspace contract.
	Workflow *workspace.Workflow
	// InstanceID is the full plugin instance ID
	// (e.g. "nexus.workflows.icm" or "nexus.workflows.icm/script").
	InstanceID string
	// DefaultBasePosture is the plugin config `default_workflow_posture`
	// name. Empty when not configured.
	DefaultBasePosture string
	// Registry is the posture registry to look up named bases against.
	Registry posture.Registry
	// SkillToolName is the instance-scoped read_skill_reference tool name.
	SkillToolName string
	// AutoIncludeSkillTool controls whether stages with non-empty
	// Inputs.Skills automatically get SkillToolName appended to their
	// AllowedTools.
	AutoIncludeSkillTool bool
}

// Build returns the derived AgentPosture for a workflow stage. The
// posture's Name is the registry key derived from PostureName.
//
// Errors propagate from base lookup (referenced posture missing) and
// operator template rendering.
func (b *PostureBuilder) Build(stage *workspace.Stage) (posture.AgentPosture, error) {
	return b.build(stage, false)
}

// BuildVerifier returns the derived AgentPosture for a verifier stage.
// Identical to Build aside from the registry key shape (which already
// matches because verifier IDs are flat strings handled the same way).
func (b *PostureBuilder) BuildVerifier(verifier *workspace.Stage) (posture.AgentPosture, error) {
	return b.build(verifier, true)
}

// PostureName returns the registry name a stage (or verifier) posture is
// registered under. Default instance: `icm.<stageID>`. Suffixed instance
// (`nexus.workflows.icm/<suffix>`): `icm.<suffix>.<stageID>`.
func (b *PostureBuilder) PostureName(stageID string) string {
	return icmtypes.StagePostureName(b.InstanceID, stageID)
}

// SchemaName returns the engine SchemaRegistry key used for a stage's
// `output` schema. Default instance: `icm.<stageID>.output`. Suffixed:
// `icm.<suffix>.<stageID>.output`.
func (b *PostureBuilder) SchemaName(stageID string) string {
	return icmtypes.StageOutputSchemaName(b.InstanceID, stageID)
}

// PredicateSchemaName returns the schema registry key used for a per-
// predicate JSON schema (output validator or loop `until` condition).
func (b *PostureBuilder) PredicateSchemaName(stageID, predicateName string) string {
	return icmtypes.PredicateSchemaName(b.InstanceID, stageID, predicateName)
}

// build is the shared implementation of Build/BuildVerifier.
func (b *PostureBuilder) build(stage *workspace.Stage, _ bool) (posture.AgentPosture, error) {
	if stage == nil {
		return posture.AgentPosture{}, errors.New("posture: stage is required")
	}
	if b.Workflow == nil {
		return posture.AgentPosture{}, errors.New("posture: workflow is required")
	}

	// 1. Seed from the plugin-level default workflow posture, when set.
	base, err := b.resolveBase(b.DefaultBasePosture)
	if err != nil {
		return posture.AgentPosture{}, fmt.Errorf("posture: default base %q: %w", b.DefaultBasePosture, err)
	}

	// 2. If the stage names a posture, it replaces the entire base for
	//    Model/AllowedTools/Budget/MaxRecursionDepth purposes.
	if stage.Agent.Posture != "" {
		named, err := b.resolveBase(stage.Agent.Posture)
		if err != nil {
			return posture.AgentPosture{}, fmt.Errorf("posture: stage %q references unknown posture %q: %w",
				stage.ID, stage.Agent.Posture, err)
		}
		if named != nil {
			base = named
		}
	}

	derived := posture.AgentPosture{
		Name:        b.PostureName(stage.ID),
		Description: deriveDescription(stage),
	}
	if base != nil {
		derived.Model = base.Model
		// Copy AllowedTools so later mutations don't bleed into the
		// registry's stored base posture.
		if len(base.AllowedTools) > 0 {
			derived.AllowedTools = append([]string(nil), base.AllowedTools...)
		}
		derived.DefaultBudget = base.DefaultBudget
		derived.MaxRecursionDepth = base.MaxRecursionDepth
	}

	// 3. Apply per-field stage overrides on top.
	applyAgentOverrides(&derived, stage.Agent)

	// 4. Render the SystemPrompt fresh — base prompt is discarded.
	prompt, err := b.renderOperatorPrompt(stage)
	if err != nil {
		return posture.AgentPosture{}, fmt.Errorf("posture: render operator template for stage %q: %w", stage.ID, err)
	}
	derived.SystemPrompt = prompt

	// 5. Output schema name when stage emits JSON.
	if stage.Output.Format == workspace.OutputJSON {
		derived.OutputSchema = b.SchemaName(stage.ID)
	}

	// 6. Auto-include the skill reference tool when the stage uses skills.
	if b.AutoIncludeSkillTool && len(stage.Inputs.Skills) > 0 && b.SkillToolName != "" {
		derived.AllowedTools = appendIfMissing(derived.AllowedTools, b.SkillToolName)
	}

	return derived, nil
}

// resolveBase looks up a posture in the registry. Empty name returns nil
// without error (no base configured).
func (b *PostureBuilder) resolveBase(name string) (*posture.AgentPosture, error) {
	if name == "" {
		return nil, nil
	}
	if b.Registry == nil {
		return nil, errors.New("registry is nil")
	}
	p, err := b.Registry.Get(name)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// renderOperatorPrompt parses the workspace operator template body and
// renders it with the supplied stage. The stage-level prompt overlay (if
// any) is appended to the rendered result so it can refine the operator
// identity without fighting the template engine.
func (b *PostureBuilder) renderOperatorPrompt(stage *workspace.Stage) (string, error) {
	tpl, err := template.New("operator").Parse(b.Workflow.Operator.Body)
	if err != nil {
		return "", err
	}
	ctx := OperatorTemplateCtx{
		Workspace: WorkspaceTplCtx{
			Name:        filepath.Base(b.Workflow.Root),
			Description: firstNonEmptyLine(b.Workflow.WorkspaceDoc),
		},
		Stage: StageTplCtx{
			ID:        stage.ID,
			Display:   stage.Display,
			Role:      stage.Role,
			Output:    outputTplCtx(stage.Output),
			HumanGate: string(stage.HumanGate),
		},
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ctx); err != nil {
		return "", err
	}
	out := buf.String()
	if stage.Agent.PromptOverlay != "" {
		out = out + "\n\n" + stage.Agent.PromptOverlay
	}
	return out, nil
}

// SkillToolName returns the instance-scoped read_skill_reference tool
// name. The default instance gets the bare name; suffixed instances get
// `read_skill_reference_<suffix>` so multi-instance ICM configurations
// don't collide on a shared tool name.
func SkillToolName(instanceID string) string {
	if suffix := icmtypes.InstanceSuffix(instanceID); suffix != "" {
		return "read_skill_reference_" + suffix
	}
	return "read_skill_reference"
}

// applyAgentOverrides layers stage AgentSpec fields onto the derived
// posture. Per-field override semantics:
//
//   - ModelRole: empty inherits, non-empty overrides.
//   - Tools: nil inherits (workspace defaults already pre-merged by the
//     loader); non-nil (including explicit empty []) replaces.
//   - Budget fields: zero inherits, positive overrides.
//   - MaxRecursionDepth: zero inherits, positive overrides.
func applyAgentOverrides(p *posture.AgentPosture, spec workspace.AgentSpec) {
	if spec.ModelRole != "" {
		p.Model.ModelRole = spec.ModelRole
	}
	if spec.Tools != nil {
		// Copy to avoid sharing the underlying slice with the workspace
		// load result. Allocate explicitly so an empty (`[]`) override
		// remains distinguishable from a nil "inherit" signal.
		tools := make([]string, len(spec.Tools))
		copy(tools, spec.Tools)
		p.AllowedTools = tools
	}
	if spec.Budget.TimeoutSeconds > 0 {
		p.DefaultBudget.Timeout = time.Duration(spec.Budget.TimeoutSeconds) * time.Second
	}
	if spec.Budget.MaxTokens > 0 {
		p.DefaultBudget.MaxTokens = spec.Budget.MaxTokens
	}
	if spec.Budget.MaxToolCalls > 0 {
		p.DefaultBudget.MaxToolCalls = spec.Budget.MaxToolCalls
	}
	if spec.MaxRecursionDepth > 0 {
		p.MaxRecursionDepth = spec.MaxRecursionDepth
	}
}

// deriveDescription returns a short human-facing description for the
// derived posture. Prefers the stage Display label; falls back to ID.
func deriveDescription(stage *workspace.Stage) string {
	if stage.Display != "" {
		return stage.Display
	}
	return stage.ID
}

// outputTplCtx flattens workspace.OutputSpec into the OperatorTemplateCtx
// shape. Format/Filename/Schema are surfaced as strings so the template
// can reference them via `{{ .Stage.Output.Format }}` etc.
func outputTplCtx(out workspace.OutputSpec) OutputTplCtx {
	return OutputTplCtx{
		Format:   string(out.Format),
		Filename: out.Filename,
		Schema:   out.Schema,
	}
}

// firstNonEmptyLine returns the first non-empty trimmed line of s.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// appendIfMissing appends v to s when not already present.
func appendIfMissing(s []string, v string) []string {
	for _, existing := range s {
		if existing == v {
			return s
		}
	}
	return append(s, v)
}
