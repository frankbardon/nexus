package icm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// TestReadSkillReference_HappyPath covers a clean read of a reference
// file under a skill's references/ subtree.
func TestReadSkillReference_HappyPath(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "screenwriting")
	refsDir := filepath.Join(skillDir, "references")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refsDir, "format.md"), []byte("# Format rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sk := &workspace.Skill{Name: "screenwriting", Path: skillDir}
	got, err := readSkillReference(sk, "format.md")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "# Format rules\n" {
		t.Fatalf("content = %q", got)
	}
}

// TestReadSkillReference_RejectsAbsolutePath ensures absolute paths
// cannot pull files outside the skill folder.
func TestReadSkillReference_RejectsAbsolutePath(t *testing.T) {
	sk := &workspace.Skill{Name: "x", Path: t.TempDir()}
	_, err := readSkillReference(sk, "/etc/passwd")
	if err == nil {
		t.Fatal("nil err for absolute path")
	}
}

// TestReadSkillReference_RejectsTraversal ensures `..` segments cannot
// escape the references/ root.
func TestReadSkillReference_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "screenwriting")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	sk := &workspace.Skill{Name: "screenwriting", Path: skillDir}
	_, err := readSkillReference(sk, "../secret.txt")
	if err == nil {
		t.Fatal("nil err for traversal path")
	}
}

// TestReadSkillReference_MissingFile surfaces a clear error when the
// requested reference doesn't exist.
func TestReadSkillReference_MissingFile(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "screenwriting")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	sk := &workspace.Skill{Name: "screenwriting", Path: skillDir}
	_, err := readSkillReference(sk, "missing.md")
	if err == nil {
		t.Fatal("nil err for missing file")
	}
}

// TestHandleSkillReferenceInvoke_UnknownSkill confirms the tool emits
// a tool.result with an Error string when the skill name isn't in any
// stage's skill map.
func TestHandleSkillReferenceInvoke_UnknownSkill(t *testing.T) {
	bus := newRecordingBus()
	p := newTestPlugin(bus)
	p.workflow = &workspace.Workflow{} // empty workspace = no skills

	p.handleSkillReferenceInvoke(events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          p.skillToolName,
		Arguments:     map[string]any{"skill": "ghost", "path": "foo.md"},
	})

	r := bus.last("tool.result")
	if r == nil {
		t.Fatal("no tool.result")
	}
	tr, _ := r.(events.ToolResult)
	if tr.Error == "" {
		t.Fatalf("expected Error; got Output=%q", tr.Output)
	}
}
