package icm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// registerSkillReferenceTool registers the per-instance
// read_skill_reference[_<suffix>] tool with the LLM, IFF any stage in
// the loaded workspace actually uses skills. Workspaces without skills
// stay tool-quiet.
func (p *Plugin) registerSkillReferenceTool() error {
	if !p.workspaceUsesSkills() {
		return nil
	}
	def := events.ToolDef{
		Name:        p.skillToolName,
		Description: "Read a skill reference file. Args: {skill: name, path: relative path under references/}. Use when a <ref/> in the active skill's <references_available> block is relevant to the current task. The skill body is already inlined in your grounding payload — only call this when you need the deeper reference.",
		Class:       "skills",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill": map[string]any{
					"type":        "string",
					"description": "Skill name as listed in the <skill name=\"...\"> block of your grounding payload.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Reference path as listed in the <ref path=\"...\"/> element of references_available. Relative to the skill's references/ folder; must not start with '/' or contain '..'.",
				},
			},
			"required": []string{"skill", "path"},
		},
	}
	return p.bus.Emit("tool.register", def)
}

// handleSkillReferenceInvoke serves the read_skill_reference tool. It
// scans every stage's resolved skills for a name match (workspace
// skills are scoped to the workspace, so the same skill resolves
// identically across stages that load it), validates the path, and
// returns the file content as tool.result.Output. On any failure mode
// the tool.result carries an Error string so the LLM can recover.
func (p *Plugin) handleSkillReferenceInvoke(tc events.ToolCall) {
	skillName, _ := tc.Arguments["skill"].(string)
	refPath, _ := tc.Arguments["path"].(string)

	emit := func(output, errMsg string) {
		res := events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            tc.ID,
			Name:          tc.Name,
			Output:        output,
			Error:         errMsg,
			TurnID:        tc.TurnID,
		}
		if veto, err := p.bus.EmitVetoable("before:tool.result", &res); err == nil && veto.Vetoed {
			return
		}
		_ = p.bus.Emit("tool.result", res)
	}

	if skillName == "" {
		emit("", "skill is required")
		return
	}
	if refPath == "" {
		emit("", "path is required")
		return
	}

	skill := p.findSkill(skillName)
	if skill == nil {
		emit("", fmt.Sprintf("skill %q not found in workspace", skillName))
		return
	}

	content, err := readSkillReference(skill, refPath)
	if err != nil {
		emit("", err.Error())
		return
	}
	emit(content, "")
}

// findSkill returns the first resolved Skill across all stages and
// verifiers that matches name. Workspace skills are content-shared so
// returning any one of them is correct.
func (p *Plugin) findSkill(name string) *workspace.Skill {
	if p.workflow == nil {
		return nil
	}
	for i := range p.workflow.Stages {
		if sk, ok := p.workflow.Stages[i].Skills[name]; ok {
			return sk
		}
	}
	for _, v := range p.workflow.Verifiers {
		if sk, ok := v.Skills[name]; ok {
			return sk
		}
	}
	return nil
}

// workspaceUsesSkills reports whether any stage or verifier declares
// inputs.skills entries.
func (p *Plugin) workspaceUsesSkills() bool {
	if p.workflow == nil {
		return false
	}
	for i := range p.workflow.Stages {
		if len(p.workflow.Stages[i].Skills) > 0 {
			return true
		}
	}
	for _, v := range p.workflow.Verifiers {
		if len(v.Skills) > 0 {
			return true
		}
	}
	return false
}

// readSkillReference resolves and reads a reference file under the
// skill's references/ subtree. Rejects absolute paths and any path
// that escapes the references/ root.
func readSkillReference(skill *workspace.Skill, ref string) (string, error) {
	if strings.HasPrefix(ref, "/") {
		return "", fmt.Errorf("invalid reference path %q: must be relative", ref)
	}
	clean := filepath.Clean(ref)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("invalid reference path %q: must not contain parent traversal", ref)
	}

	refsRoot := filepath.Join(skill.Path, "references")
	abs := filepath.Join(refsRoot, clean)

	absRefs, err := filepath.Abs(refsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve references root: %w", err)
	}
	absResolved, err := filepath.Abs(abs)
	if err != nil {
		return "", fmt.Errorf("resolve reference path: %w", err)
	}
	if !strings.HasPrefix(absResolved+string(os.PathSeparator), absRefs+string(os.PathSeparator)) &&
		absResolved != absRefs {
		return "", fmt.Errorf("reference path escapes references root")
	}

	data, err := os.ReadFile(absResolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("reference %q not found in skill %q", ref, skill.Name)
		}
		return "", fmt.Errorf("read reference: %w", err)
	}
	return string(data), nil
}
