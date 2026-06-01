package icm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestWorkspaceName(t *testing.T) {
	cases := map[string]string{
		"/tmp/workflows/script-pipeline":  "script-pipeline",
		"/tmp/workflows/script-pipeline/": "script-pipeline",
		"workspace":                       "workspace",
	}
	for in, want := range cases {
		if got := workspaceName(in); got != want {
			t.Errorf("workspaceName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"\n\n":                        "",
		"line1\nline2":                "line1",
		"   leading whitespace\nrest": "leading whitespace",
		"\n   line after empty\n":     "line after empty",
		"no newline at all":           "no newline at all",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFormatLoadErrors_PassThrough(t *testing.T) {
	if got := formatLoadErrors(nil); got != "ok" {
		t.Fatalf("nil err format = %q; want ok", got)
	}
}

// TestHandleValidateInvoke_OK exercises the tool surface against a
// minimal but valid workspace on disk. The handler should emit
// tool.result with Output == "ok".
func TestHandleValidateInvoke_OK(t *testing.T) {
	dir := buildMinimalWorkspace(t)
	bus := newRecordingBus()
	p := newTestPlugin(bus)
	p.cfg.Workspace = dir

	p.handleValidateInvoke(events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          p.validateToolName(),
		TurnID:        "turn-x",
	})

	r := bus.last("tool.result")
	if r == nil {
		t.Fatal("no tool.result emitted")
	}
	tr, _ := r.(events.ToolResult)
	if tr.Output != "ok" {
		t.Fatalf("Output = %q; want ok", tr.Output)
	}
	if tr.ID != "call-1" {
		t.Fatalf("ID = %q", tr.ID)
	}
}

// TestHandleValidateInvoke_Error proves a workspace missing required
// files surfaces errors verbatim.
func TestHandleValidateInvoke_Error(t *testing.T) {
	dir := t.TempDir() // empty dir is an invalid workspace
	bus := newRecordingBus()
	p := newTestPlugin(bus)
	p.cfg.Workspace = dir

	p.handleValidateInvoke(events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-2",
		Name:          p.validateToolName(),
	})

	r := bus.last("tool.result")
	if r == nil {
		t.Fatal("no tool.result emitted")
	}
	tr, _ := r.(events.ToolResult)
	if tr.Output == "ok" {
		t.Fatalf("Output = %q; expected loader errors", tr.Output)
	}
	if !strings.Contains(tr.Output, "workspace") && !strings.Contains(tr.Output, "stages") {
		t.Fatalf("Output %q does not mention workspace/stages", tr.Output)
	}
}

// buildMinimalWorkspace creates a one-stage workspace sufficient to
// pass the loader. Returns the absolute path.
func buildMinimalWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	must := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	must(filepath.Join(root, "workspace.md"), "# test workspace\n\nA minimal hermetic workspace.\n")
	must(filepath.Join(root, "operator.md"), "# Operator\n\nDo the task.\n")
	must(filepath.Join(root, "stages", "01_only", "contract.md"), `---
turns:
  policy: fixed
  max: 1
output:
  format: text
  filename: out.txt
  persist: file_ref
---

You are the only stage.
`)
	return root
}
