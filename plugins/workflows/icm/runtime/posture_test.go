package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// defaultOperatorTemplate mirrors defaults/operator.md closely enough that
// the rendered prompt depends on the Workspace + Stage ctx. It deliberately
// references both halves so test failures pinpoint missing data.
const defaultOperatorTemplate = `# ICM Operator
You are an ICM operator executing stage {{ .Stage.ID }} of {{ .Workspace.Name }}.
Description: {{ .Workspace.Description }}
Display: {{ .Stage.Display }}
Role: {{ .Stage.Role }}
Output: format={{ .Stage.Output.Format }} filename={{ .Stage.Output.Filename }}
HumanGate: {{ .Stage.HumanGate }}
`

// newWorkflow returns a minimal workflow rooted in tmp with the given
// operator template body. workspace.md content shapes the rendered
// Workspace.Description (first non-empty line).
func newWorkflow(t *testing.T, operatorBody, workspaceDoc string) *workspace.Workflow {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "demo_workspace")
	return &workspace.Workflow{
		Root:         root,
		Operator:     workspace.OperatorConfig{Body: operatorBody, Source: "default"},
		WorkspaceDoc: workspaceDoc,
	}
}

func newStage(id string, mod ...func(*workspace.Stage)) *workspace.Stage {
	s := &workspace.Stage{
		ID:      id,
		Display: id + " display",
		Role:    "Stage " + id + " role body.",
		Output: workspace.OutputSpec{
			Format:   workspace.OutputText,
			Filename: id + ".md",
			Persist:  workspace.PersistFileRef,
		},
	}
	for _, m := range mod {
		m(s)
	}
	return s
}

// --- Tests -----------------------------------------------------------------

// 1. Build with no base posture and no stage overrides yields a posture
// whose Model/AllowedTools/Budget fields are all zero values.
func TestBuild_NoBaseNoOverrides(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "Demo workflow\nmore text\n")
	stage := newStage("01_draft")

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Name != "icm.01_draft" {
		t.Errorf("Name = %q, want icm.01_draft", got.Name)
	}
	if got.Model.ModelRole != "" || got.Model.Provider != "" {
		t.Errorf("Model not zero: %+v", got.Model)
	}
	if got.AllowedTools != nil {
		t.Errorf("AllowedTools = %v, want nil", got.AllowedTools)
	}
	if got.DefaultBudget != (posture.ResourceBudget{}) {
		t.Errorf("Budget not zero: %+v", got.DefaultBudget)
	}
	if !strings.Contains(got.SystemPrompt, "executing stage 01_draft") {
		t.Errorf("SystemPrompt missing stage ID: %s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Demo workflow") {
		t.Errorf("SystemPrompt missing workspace description first line: %s", got.SystemPrompt)
	}
}

// 2. Stage agent.model_role only populates the derived ModelRole.
func TestBuild_ModelRoleOnly(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	stage := newStage("02_script", func(s *workspace.Stage) {
		s.Agent.ModelRole = "writer"
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Model.ModelRole != "writer" {
		t.Errorf("ModelRole = %q, want writer", got.Model.ModelRole)
	}
}

// 3. Stage agent.tools = [] (explicit empty, non-nil) yields explicit empty
// AllowedTools — never inheriting from the base.
func TestBuild_ExplicitEmptyTools(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	reg := posture.NewRegistry()
	if err := reg.Register(posture.AgentPosture{
		Name:         "base",
		AllowedTools: []string{"read_file", "write_file"},
	}); err != nil {
		t.Fatalf("Register base: %v", err)
	}

	stage := newStage("03_only", func(s *workspace.Stage) {
		s.Agent.Posture = "base"
		s.Agent.Tools = []string{} // explicit opt-out
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   reg,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.AllowedTools == nil {
		t.Errorf("AllowedTools = nil, want non-nil empty (explicit opt-out)")
	}
	if len(got.AllowedTools) != 0 {
		t.Errorf("AllowedTools = %v, want empty", got.AllowedTools)
	}
}

// 4. Stage agent.tools = nil (absent) inherits the base posture's tools.
func TestBuild_NilToolsInherits(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	reg := posture.NewRegistry()
	if err := reg.Register(posture.AgentPosture{
		Name:         "base",
		AllowedTools: []string{"read_file", "write_file"},
	}); err != nil {
		t.Fatalf("Register base: %v", err)
	}

	stage := newStage("04_inherit", func(s *workspace.Stage) {
		s.Agent.Posture = "base"
		// s.Agent.Tools left nil
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   reg,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.AllowedTools) != 2 || got.AllowedTools[0] != "read_file" || got.AllowedTools[1] != "write_file" {
		t.Errorf("AllowedTools = %v, want [read_file write_file]", got.AllowedTools)
	}
}

// 5. Cascade: base provides Model.MaxTokens; stage overrides ModelRole —
// both surface in the derived posture.
func TestBuild_CascadeBaseAndStage(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	reg := posture.NewRegistry()
	if err := reg.Register(posture.AgentPosture{
		Name: "base",
		Model: posture.ModelConfig{
			MaxTokens:   4000,
			Temperature: 0.3,
		},
		DefaultBudget: posture.ResourceBudget{
			Timeout:   30 * time.Second,
			MaxTokens: 2000,
		},
		MaxRecursionDepth: 2,
	}); err != nil {
		t.Fatalf("Register base: %v", err)
	}

	stage := newStage("05_cascade", func(s *workspace.Stage) {
		s.Agent.Posture = "base"
		s.Agent.ModelRole = "reviewer"
		s.Agent.Budget.TimeoutSeconds = 60 // override Timeout
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   reg,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.Model.ModelRole != "reviewer" {
		t.Errorf("ModelRole = %q, want reviewer", got.Model.ModelRole)
	}
	if got.Model.MaxTokens != 4000 {
		t.Errorf("Model.MaxTokens = %d, want 4000 (from base)", got.Model.MaxTokens)
	}
	if got.Model.Temperature != 0.3 {
		t.Errorf("Model.Temperature = %v, want 0.3 (from base)", got.Model.Temperature)
	}
	if got.DefaultBudget.Timeout != 60*time.Second {
		t.Errorf("Budget.Timeout = %v, want 60s (override)", got.DefaultBudget.Timeout)
	}
	if got.DefaultBudget.MaxTokens != 2000 {
		t.Errorf("Budget.MaxTokens = %d, want 2000 (from base)", got.DefaultBudget.MaxTokens)
	}
	if got.MaxRecursionDepth != 2 {
		t.Errorf("MaxRecursionDepth = %d, want 2 (from base)", got.MaxRecursionDepth)
	}
}

// 6. Stage has Inputs.Skills and AutoIncludeSkillTool=true → AllowedTools
// gains the skill tool.
func TestBuild_AutoIncludeSkillTool(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	stage := newStage("06_skills", func(s *workspace.Stage) {
		s.Inputs.Skills = []string{"writing"}
		s.Agent.Tools = []string{"read_file"}
	})

	b := &PostureBuilder{
		Workflow:             wf,
		InstanceID:           "nexus.workflows.icm",
		Registry:             posture.NewRegistry(),
		SkillToolName:        "read_skill_reference",
		AutoIncludeSkillTool: true,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !containsTool(got.AllowedTools, "read_skill_reference") {
		t.Errorf("AllowedTools missing skill tool: %v", got.AllowedTools)
	}
	if !containsTool(got.AllowedTools, "read_file") {
		t.Errorf("AllowedTools missing original tool: %v", got.AllowedTools)
	}
}

// 7. Skill tool already in AllowedTools → no duplicate added.
func TestBuild_SkillToolNoDuplicate(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	stage := newStage("07_dup", func(s *workspace.Stage) {
		s.Inputs.Skills = []string{"writing"}
		s.Agent.Tools = []string{"read_skill_reference", "read_file"}
	})

	b := &PostureBuilder{
		Workflow:             wf,
		InstanceID:           "nexus.workflows.icm",
		Registry:             posture.NewRegistry(),
		SkillToolName:        "read_skill_reference",
		AutoIncludeSkillTool: true,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	count := 0
	for _, tool := range got.AllowedTools {
		if tool == "read_skill_reference" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("skill tool appears %d times, want exactly 1: %v", count, got.AllowedTools)
	}
}

// 8. No skills + AutoIncludeSkillTool=true → skill tool NOT added.
func TestBuild_NoSkillsNoTool(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	stage := newStage("08_noskills", func(s *workspace.Stage) {
		s.Agent.Tools = []string{"read_file"}
	})

	b := &PostureBuilder{
		Workflow:             wf,
		InstanceID:           "nexus.workflows.icm",
		Registry:             posture.NewRegistry(),
		SkillToolName:        "read_skill_reference",
		AutoIncludeSkillTool: true,
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if containsTool(got.AllowedTools, "read_skill_reference") {
		t.Errorf("AllowedTools unexpectedly contains skill tool: %v", got.AllowedTools)
	}
}

// 9. PostureName: default instance vs suffixed.
func TestPostureName(t *testing.T) {
	cases := []struct {
		instanceID string
		stageID    string
		want       string
	}{
		{"nexus.workflows.icm", "02_script", "icm.02_script"},
		{"nexus.workflows.icm/script", "02_script", "icm.script.02_script"},
		{"nexus.workflows.icm/research", "verify_facts", "icm.research.verify_facts"},
	}
	for _, c := range cases {
		t.Run(c.instanceID+"/"+c.stageID, func(t *testing.T) {
			b := &PostureBuilder{InstanceID: c.instanceID}
			if got := b.PostureName(c.stageID); got != c.want {
				t.Errorf("PostureName(%q) = %q, want %q", c.stageID, got, c.want)
			}
		})
	}
}

// 10. OutputSchema populated when format=json; empty when format=text.
func TestBuild_OutputSchemaField(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	jsonStage := newStage("09_json", func(s *workspace.Stage) {
		s.Output.Format = workspace.OutputJSON
		s.Output.Schema = "schemas/out.json"
	})
	gotJSON, err := b.Build(jsonStage)
	if err != nil {
		t.Fatalf("Build json: %v", err)
	}
	if gotJSON.OutputSchema != "icm.09_json.output" {
		t.Errorf("OutputSchema = %q, want icm.09_json.output", gotJSON.OutputSchema)
	}

	textStage := newStage("10_text")
	gotText, err := b.Build(textStage)
	if err != nil {
		t.Fatalf("Build text: %v", err)
	}
	if gotText.OutputSchema != "" {
		t.Errorf("OutputSchema = %q, want empty for text stage", gotText.OutputSchema)
	}
}

// 11. Operator template renders Workspace + Stage; referencing a Run var
// causes a render error (Run is NOT exposed).
func TestBuild_NoRunVarInTemplate(t *testing.T) {
	tpl := "Stage {{ .Stage.ID }} run {{ .Run.ID }}"
	wf := newWorkflow(t, tpl, "doc\n")
	stage := newStage("11_norun")

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	_, err := b.Build(stage)
	if err == nil {
		t.Fatalf("Build: expected error referencing .Run, got nil")
	}
	if !strings.Contains(err.Error(), "Run") {
		t.Errorf("error %q does not mention Run", err.Error())
	}
}

// 12. SkillToolName helper handles default vs suffixed instances.
func TestSkillToolName(t *testing.T) {
	cases := []struct {
		instanceID string
		want       string
	}{
		{"nexus.workflows.icm", "read_skill_reference"},
		{"nexus.workflows.icm/script", "read_skill_reference_script"},
		{"nexus.workflows.icm/research", "read_skill_reference_research"},
		{"", "read_skill_reference"},
	}
	for _, c := range cases {
		t.Run(c.instanceID, func(t *testing.T) {
			if got := SkillToolName(c.instanceID); got != c.want {
				t.Errorf("SkillToolName(%q) = %q, want %q", c.instanceID, got, c.want)
			}
		})
	}
}

// 13. Stage prompt overlay is appended to the rendered template.
func TestBuild_PromptOverlayAppended(t *testing.T) {
	wf := newWorkflow(t, "Base prompt {{ .Stage.ID }}.", "doc\n")
	stage := newStage("13_overlay", func(s *workspace.Stage) {
		s.Agent.PromptOverlay = "Additional stage-level guidance."
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	got, err := b.Build(stage)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got.SystemPrompt, "Base prompt 13_overlay.") {
		t.Errorf("SystemPrompt missing rendered base: %s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Additional stage-level guidance.") {
		t.Errorf("SystemPrompt missing overlay: %s", got.SystemPrompt)
	}
}

// 14. Referencing a stage-named posture that doesn't exist surfaces an
// error from Build.
func TestBuild_MissingNamedPostureErrors(t *testing.T) {
	wf := newWorkflow(t, defaultOperatorTemplate, "doc\n")
	stage := newStage("14_missing", func(s *workspace.Stage) {
		s.Agent.Posture = "nonexistent"
	})

	b := &PostureBuilder{
		Workflow:   wf,
		InstanceID: "nexus.workflows.icm",
		Registry:   posture.NewRegistry(),
	}

	_, err := b.Build(stage)
	if err == nil {
		t.Fatalf("Build: expected error for missing posture, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q does not mention missing posture name", err.Error())
	}
}

// --- helpers --------------------------------------------------------------

func containsTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}
