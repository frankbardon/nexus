package codeexec

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestBuildBindingsSection_MirrorsLiveRegistry proves the system-prompt
// section lists the real `tools.*` names derived from registered tools.
// Prior regression: a static contract hinted "file_read → tools.FileRead"
// and the LLM called that binding when the tool was actually named
// "read_file". A dynamic list kills that failure mode.
func TestBuildBindingsSection_MirrorsLiveRegistry(t *testing.T) {
	bus := engine.NewEventBus()
	prompts := engine.NewPromptRegistry()

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Prompts: prompts,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	_ = bus.Emit("tool.register", events.ToolDef{
		Name:       "read_file",
		Parameters: map[string]any{"type": "object"},
	})
	_ = bus.Emit("tool.register", events.ToolDef{
		Name:       "shell",
		Parameters: map[string]any{"type": "object"},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"stdout": map[string]any{"type": "string"},
			},
		},
	})

	section := p.buildBindingsSection()

	if !strings.Contains(section, "tools.ReadFile(tools.ReadFileArgs)") {
		t.Errorf("section missing tools.ReadFile binding:\n%s", section)
	}
	if !strings.Contains(section, "tools.Shell(tools.ShellArgs)") {
		t.Errorf("section missing tools.Shell binding:\n%s", section)
	}
	if !strings.Contains(section, "tools.ShellResult (typed from OutputSchema)") {
		t.Errorf("section missing typed-result annotation for shell:\n%s", section)
	}
	if !strings.Contains(section, "tools.Result{Output, Error, OutputFile}") {
		t.Errorf("section missing schema-less fallback note:\n%s", section)
	}
	if strings.Contains(section, "tools.RunCode") || strings.Contains(section, "run_code") {
		t.Errorf("bindings section must not list run_code itself:\n%s", section)
	}

	// Applying the registry must include this section verbatim.
	applied := prompts.Apply("")
	if !strings.Contains(applied, "tools.ReadFile(tools.ReadFileArgs)") {
		t.Errorf("PromptRegistry.Apply lost the bindings section: %s", applied)
	}
}
