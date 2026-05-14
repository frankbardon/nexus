//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
	mcpclient "github.com/frankbardon/nexus/plugins/mcp/client"
)

var (
	fakeBinOnce sync.Once
	fakeBinPath string
)

// buildFakeMCPServer compiles tests/integration/mcp_fake/ to a temp binary
// the first time it's needed, then caches the path for subsequent tests.
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	fakeBinOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "mcp-fake-*")
		if err != nil {
			t.Fatalf("mkdir fake-bin: %v", err)
		}
		bin := filepath.Join(tmpDir, "mcp_fake")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		root := findRoot(t)
		cmd := exec.Command("go", "build", "-o", bin, "./tests/integration/mcp_fake/")
		cmd.Dir = root
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build fake mcp server: %v\n%s", err, out)
		}
		fakeBinPath = bin
	})
	return fakeBinPath
}

// newMCPHarness boots the nexus.mcp.client plugin with a config pointing at
// the fake stdio server. Returns the contract harness and a helper to wait
// briefly for the async connect goroutines to settle.
func newMCPHarness(t *testing.T) (*contract.ContractHarness, func()) {
	t.Helper()
	bin := buildFakeMCPServer(t)
	h := contract.NewContract(t, mcpclient.New, contract.WithPluginConfig(map[string]any{
		"servers": []any{
			map[string]any{
				"name":      "fake",
				"transport": "stdio",
				"command":   bin,
				"lifecycle": "engine",
				"timeout":   "10s",
				"resources": map[string]any{
					"enabled":              true,
					"auto_register_static": true,
					"auto_register_max":    50,
					"subscribe_updates":    false,
				},
				"prompts": map[string]any{"enabled": true},
			},
		},
	}))
	settle := func() { time.Sleep(150 * time.Millisecond) }
	settle()
	return h, settle
}

// TestMCPClient_RegistersTools verifies the plugin emits tool.register for
// every MCP tool returned by the fake server.
func TestMCPClient_RegistersTools(t *testing.T) {
	h, _ := newMCPHarness(t)

	var names []string
	for _, ev := range h.Captured() {
		if ev.Type != "tool.register" {
			continue
		}
		td, ok := ev.Payload.(events.ToolDef)
		if !ok {
			continue
		}
		names = append(names, td.Name)
	}

	wantPresent := []string{
		"mcp__fake__echo",
		"mcp__fake__add",
		"mcp__fake__list_resources",
		"mcp__fake__read_resource",
	}
	for _, w := range wantPresent {
		if !containsString(names, w) {
			t.Errorf("missing tool registration %q (have %v)", w, names)
		}
	}
}

// TestMCPClient_AutoRegistersStaticResources checks both the readme and the
// pixel resource land in the catalog as no-arg tools with the slug suffix.
func TestMCPClient_AutoRegistersStaticResources(t *testing.T) {
	h, _ := newMCPHarness(t)

	var names []string
	for _, ev := range h.Captured() {
		if ev.Type != "tool.register" {
			continue
		}
		td, ok := ev.Payload.(events.ToolDef)
		if !ok {
			continue
		}
		if startsWithStr(td.Name, "mcp__fake__resource__") {
			names = append(names, td.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 static resources, got %v", names)
	}
}

// TestMCPClient_AutoRegistersTemplate validates the URI-template surface
// (with its declared variable) shows up as a tool with the right input
// schema.
func TestMCPClient_AutoRegistersTemplate(t *testing.T) {
	h, _ := newMCPHarness(t)

	var found *events.ToolDef
	for _, ev := range h.Captured() {
		if ev.Type != "tool.register" {
			continue
		}
		td, ok := ev.Payload.(events.ToolDef)
		if !ok {
			continue
		}
		if startsWithStr(td.Name, "mcp__fake__template__") {
			found = &td
			break
		}
	}
	if found == nil {
		t.Fatal("expected template tool to be registered")
	}
	props, _ := found.Parameters["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Fatalf("template tool should declare 'name' property, got %v", props)
	}
}

// TestMCPClient_InvokesToolThroughBus exercises the round trip via the
// shared event bus: tool.invoke "mcp__fake__echo" should produce a
// tool.result whose Output matches the argument.
func TestMCPClient_InvokesToolThroughBus(t *testing.T) {
	h, _ := newMCPHarness(t)

	var (
		mu     sync.Mutex
		result events.ToolResult
		done   = make(chan struct{}, 1)
	)
	h.Bus().Subscribe("tool.result", func(ev engine.Event[any]) {
		r, ok := ev.Payload.(events.ToolResult)
		if !ok || r.Name != "mcp__fake__echo" {
			return
		}
		mu.Lock()
		result = r
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	}, engine.WithSource("test"))

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "mcp__fake__echo",
		Arguments:     map[string]any{"text": "hi there"},
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tool.result")
	}

	mu.Lock()
	defer mu.Unlock()
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.Output != "hi there" {
		t.Fatalf("echo output: got %q want %q", result.Output, "hi there")
	}
}

// TestMCPClient_ReadResourceProducesMultimodalPart validates the binary
// resource path: pixel image should come back as an OutputPart with
// MimeType image/png (no inline-cutoff blob path here because the payload
// is well under the 64 KB threshold).
func TestMCPClient_ReadResourceProducesMultimodalPart(t *testing.T) {
	h, _ := newMCPHarness(t)

	var (
		mu     sync.Mutex
		result events.ToolResult
		done   = make(chan struct{}, 1)
	)
	h.Bus().Subscribe("tool.result", func(ev engine.Event[any]) {
		r, ok := ev.Payload.(events.ToolResult)
		if !ok || r.Name != "mcp__fake__read_resource" {
			return
		}
		mu.Lock()
		result = r
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	}, engine.WithSource("test"))

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-2",
		Name:          "mcp__fake__read_resource",
		Arguments:     map[string]any{"uri": "img://pixel"},
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for tool.result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(result.OutputParts) == 0 {
		t.Fatalf("expected multimodal parts, got Output=%q", result.Output)
	}
	if result.OutputParts[0].MimeType != "image/png" {
		t.Fatalf("first part mime: %q", result.OutputParts[0].MimeType)
	}
}

// TestMCPClient_PromptSlashExpansion verifies the slash-command intercept
// pipeline: typing "/mcp.fake.review topic=plan" vetoes the original
// io.input and re-emits a new io.input whose PreloadMessages carry the
// three messages the fake server returns from prompts/get.
func TestMCPClient_PromptSlashExpansion(t *testing.T) {
	h, _ := newMCPHarness(t)

	var (
		mu       sync.Mutex
		captured events.UserInput
		gotInput = make(chan struct{}, 1)
	)
	h.Bus().Subscribe("io.input", func(ev engine.Event[any]) {
		ui, ok := ev.Payload.(events.UserInput)
		if !ok {
			return
		}
		if len(ui.PreloadMessages) == 0 {
			return
		}
		mu.Lock()
		captured = ui
		mu.Unlock()
		select {
		case gotInput <- struct{}{}:
		default:
		}
	}, engine.WithSource("test"))

	veto := h.InjectVetoable("before:io.input", &events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "/mcp.fake.review topic=plan",
	})
	if !veto.Vetoed {
		t.Fatalf("expected veto on slash command, got %+v", veto)
	}

	select {
	case <-gotInput:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for prompt-expanded io.input")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured.PreloadMessages) != 3 {
		t.Fatalf("expected 3 preload messages, got %d (%v)", len(captured.PreloadMessages), captured.PreloadMessages)
	}
	wantRoles := []string{"user", "assistant", "user"}
	for i, want := range wantRoles {
		if captured.PreloadMessages[i].Role != want {
			t.Fatalf("preload[%d].Role = %q want %q", i, captured.PreloadMessages[i].Role, want)
		}
	}
}

// TestMCPClient_PromptsListQueryReturnsRegistry validates that the
// synchronous mcp.prompts.list query fills its payload with the slash
// commands the plugin is routing.
func TestMCPClient_PromptsListQueryReturnsRegistry(t *testing.T) {
	h, _ := newMCPHarness(t)

	q := &events.MCPPromptsList{SchemaVersion: events.MCPPromptsListVersion}
	if err := h.Bus().Emit("mcp.prompts.list", q); err != nil {
		t.Fatalf("emit query: %v", err)
	}
	if len(q.Prompts) < 2 {
		t.Fatalf("expected at least 2 prompts in registry, got %d", len(q.Prompts))
	}
	var sawGreet, sawReview bool
	for _, p := range q.Prompts {
		switch p.Prompt {
		case "greet":
			sawGreet = true
		case "review":
			sawReview = true
		}
	}
	if !sawGreet || !sawReview {
		t.Fatalf("missing prompts in registry: %v", q.Prompts)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func startsWithStr(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
