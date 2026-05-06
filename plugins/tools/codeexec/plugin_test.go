package codeexec

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// testHarness wires a real engine.EventBus to a code_exec plugin plus a fake
// downstream tool that answers "echo" calls. Tests drive the plugin by
// emitting tool.invoke("run_code", ...) and reading the resulting
// tool.result.
type testHarness struct {
	t       *testing.T
	bus     engine.EventBus
	plugin  *Plugin
	session *engine.SessionWorkspace

	results      []events.ToolResult
	codeResults  []events.CodeExecResult
	codeRequests []events.CodeExecRequest
	mu           sync.Mutex
}

func newHarness(t *testing.T, cfg map[string]any) *testHarness {
	t.Helper()
	bus := engine.NewEventBus()

	sessionDir := t.TempDir()
	sess := &engine.SessionWorkspace{
		ID:        "test",
		RootDir:   sessionDir,
		StartedAt: time.Now(),
	}

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config:  cfg,
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: sess,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	h := &testHarness{t: t, bus: bus, plugin: p, session: sess}

	// Subscribe to run_code's own tool.result (so tests can observe outcomes).
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		res, _ := e.Payload.(events.ToolResult)
		if res.Name != toolName {
			return
		}
		h.mu.Lock()
		h.results = append(h.results, res)
		h.mu.Unlock()
	}, engine.WithPriority(90), engine.WithSource("test-collector"))

	bus.Subscribe("code.exec.result", func(e engine.Event[any]) {
		r, _ := e.Payload.(events.CodeExecResult)
		h.mu.Lock()
		h.codeResults = append(h.codeResults, r)
		h.mu.Unlock()
	}, engine.WithPriority(90), engine.WithSource("test-collector"))

	bus.Subscribe("code.exec.request", func(e engine.Event[any]) {
		r, _ := e.Payload.(events.CodeExecRequest)
		h.mu.Lock()
		h.codeRequests = append(h.codeRequests, r)
		h.mu.Unlock()
	}, engine.WithPriority(90), engine.WithSource("test-collector"))

	return h
}

// registerFakeTool hooks a simple echo tool onto the bus. Its schema has one
// required "message" field. Responses echo the message back as the Output.
func (h *testHarness) registerFakeTool() {
	_ = h.bus.Emit("tool.register", events.ToolDef{
		Name:        "echo",
		Description: "Echo a message back",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
			"required": []any{"message"},
		},
	})
	h.bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, ok := e.Payload.(events.ToolCall)
		if !ok || tc.Name != "echo" {
			return
		}
		msg, _ := tc.Arguments["message"].(string)
		_ = h.bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
			Name:   tc.Name,
			Output: "echoed: " + msg,
			TurnID: tc.TurnID,
		})
	}, engine.WithPriority(40), engine.WithSource("fake-echo"))
}

func (h *testHarness) runCode(script string) events.ToolResult {
	h.t.Helper()
	// Arbitrary test ID + turn.
	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "run-" + h.t.Name(),
		Name:      toolName,
		Arguments: map[string]any{"script": script},
		TurnID:    "turn-1",
	}
	_ = h.bus.Emit("tool.invoke", tc)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) == 0 {
		h.t.Fatalf("no tool.result received; script=\n%s", script)
	}
	return h.results[len(h.results)-1]
}

// --- tests -----------------------------------------------------------------

func TestPlugin_HappyPath_EchoRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()

	script := `package main

import (
	"context"
	"fmt"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	r, err := tools.Echo(tools.EchoArgs{Message: "hi"})
	if err != nil {
		return nil, err
	}
	fmt.Println("script-says:", r.Output)
	return map[string]string{"got": r.Output}, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}

	var env map[string]any
	if err := json.Unmarshal([]byte(res.Output), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; output=%q", err, res.Output)
	}
	if !strings.Contains(env["stdout"].(string), "script-says: echoed: hi") {
		t.Errorf("stdout missing expected line: %v", env["stdout"])
	}
	result, _ := env["result"].(map[string]any)
	if result["got"] != "echoed: hi" {
		t.Errorf("result payload wrong: %v", result)
	}
}

func TestPlugin_PersistsArtifactsToSession(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()

	script := `package main
import (
	"context"
	"tools"
)
func Run(ctx context.Context) (any, error) {
	r, _ := tools.Echo(tools.EchoArgs{Message: "hi"})
	return r.Output, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}

	base := filepath.Join(h.session.RootDir, "plugins", pluginID, res.ID)
	for _, fn := range []string{"script.go", "result.json"} {
		path := filepath.Join(base, fn)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s: %v", path, err)
		}
	}
	scriptBytes, _ := os.ReadFile(filepath.Join(base, "script.go"))
	if !strings.Contains(string(scriptBytes), "tools.EchoArgs") {
		t.Errorf("persisted script does not match input")
	}
}

// TestPlugin_PersistsScriptOnRuntimeFailure pins the regression where the
// script never landed on disk if execution failed (and, for the wasm path,
// even on success). Persistence now happens up-front in runScript before
// backend dispatch, so the script is recoverable regardless of outcome.
func TestPlugin_PersistsScriptOnRuntimeFailure(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import "context"
func Run(ctx context.Context) (any, error) {
	notAVariable.Call()
	return nil, nil
}
`
	res := h.runCode(script)
	if res.Error == "" {
		t.Fatalf("expected runtime/compile error, got success: %q", res.Output)
	}

	scriptPath := filepath.Join(h.session.RootDir, "plugins", pluginID, res.ID, "script.go")
	got, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("script.go missing on failure path: %v", err)
	}
	if string(got) != script {
		t.Errorf("persisted script differs from input")
	}
}

func TestPlugin_RejectsGoroutines(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import "context"
func Run(ctx context.Context) (any, error) {
	go func() {}()
	return nil, nil
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, "go statements") {
		t.Fatalf("want go-stmt rejection, got %q", res.Error)
	}
}

func TestPlugin_RejectsDisallowedImport(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import (
	"context"
	"os"
)
func Run(ctx context.Context) (any, error) {
	_ = os.Getenv("HOME")
	return nil, nil
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, `"os"`) {
		t.Fatalf("want os rejection, got %q", res.Error)
	}
}

func TestPlugin_CompileError(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import "context"
func Run(ctx context.Context) (any, error) {
	notAVariable.Call()
	return nil, nil
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, "compile error") && !strings.Contains(res.Error, "runtime error") {
		t.Fatalf("want compile/runtime error, got %q", res.Error)
	}
}

func TestPlugin_RuntimePanic(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import "context"
func Run(ctx context.Context) (any, error) {
	var s []int
	_ = s[5]
	return nil, nil
}
`
	res := h.runCode(script)
	if res.Error == "" {
		t.Fatalf("want error for panic, got none")
	}
	if !strings.Contains(res.Error, "runtime error") && !strings.Contains(res.Error, "panic") {
		t.Errorf("unexpected error shape: %q", res.Error)
	}
}

func TestPlugin_ScriptReturnsError(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import (
	"context"
	"errors"
)
func Run(ctx context.Context) (any, error) {
	return nil, errors.New("boom")
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, "boom") {
		t.Fatalf("want boom, got %q", res.Error)
	}
}

func TestPlugin_Timeout(t *testing.T) {
	h := newHarness(t, map[string]any{
		"timeout_seconds": 1,
	})
	h.registerFakeTool()

	// Use a tight spin loop that also checks ctx.Err() so it can bail; the
	// point of the test is that once the invocation ctx cancels, tools.*
	// calls (or the ctx check) surface the error.
	script := `package main
import (
	"context"
	"time"
)
func Run(ctx context.Context) (any, error) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return "should not finish", nil
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, "timed out") && !strings.Contains(res.Error, "deadline") {
		t.Fatalf("want timeout, got %q", res.Error)
	}
}

func TestPlugin_GateVetoSurfacedAsToolError(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()

	// Install a gate that vetoes every echo invocation.
	h.bus.Subscribe("before:tool.invoke", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		tc, ok := vp.Original.(*events.ToolCall)
		if !ok || tc.Name != "echo" {
			return
		}
		vp.Veto.Vetoed = true
		vp.Veto.Reason = "denied by test gate"
	}, engine.WithPriority(10), engine.WithSource("test-gate"))

	script := `package main
import (
	"context"
	"tools"
)
func Run(ctx context.Context) (any, error) {
	_, err := tools.Echo(tools.EchoArgs{Message: "hi"})
	return nil, err
}
`
	res := h.runCode(script)
	if !strings.Contains(res.Error, "denied by test gate") && !strings.Contains(res.Error, "vetoed") {
		t.Fatalf("want gate veto, got %q", res.Error)
	}
}

func TestPlugin_EmitsCodeExecEvents(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()

	script := `package main
import (
	"context"
	"tools"
)
func Run(ctx context.Context) (any, error) {
	r, _ := tools.Echo(tools.EchoArgs{Message: "hi"})
	return r.Output, nil
}
`
	_ = h.runCode(script)
	if len(h.codeRequests) != 1 {
		t.Fatalf("want 1 code.exec.request, got %d", len(h.codeRequests))
	}
	if !strings.Contains(h.codeRequests[0].Script, "tools.Echo") {
		t.Errorf("request Script not captured")
	}
	if len(h.codeResults) != 1 {
		t.Fatalf("want 1 code.exec.result, got %d", len(h.codeResults))
	}
	if h.codeResults[0].CallID != h.codeRequests[0].CallID {
		t.Errorf("call ID mismatch: request=%q result=%q",
			h.codeRequests[0].CallID, h.codeResults[0].CallID)
	}
	if h.codeResults[0].Error != "" {
		t.Errorf("expected no error, got %q", h.codeResults[0].Error)
	}
}

func TestPlugin_LoadsSkillHelpers(t *testing.T) {
	h := newHarness(t, nil)

	// Stand up a tiny on-disk skill with one helper file.
	skillDir := t.TempDir()
	helperSrc := `package helpers

func Double(x int) int { return x * 2 }
`
	if err := os.WriteFile(filepath.Join(skillDir, "util.go"), []byte(helperSrc), 0644); err != nil {
		t.Fatal(err)
	}
	_ = h.bus.Emit("skill.loaded", events.SkillContent{SchemaVersion: events.SkillContentVersion, Name: "math-helpers",
		BaseDir: skillDir,
	})

	script := `package main

import (
	"context"
	"fmt"

	helpers "skills/math-helpers"
)

func Run(ctx context.Context) (any, error) {
	return fmt.Sprintf("doubled=%d", helpers.Double(21)), nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	if env["result"] != "doubled=42" {
		t.Errorf("want doubled=42, got %v", env["result"])
	}
}

func TestPlugin_SkillHelpersUnloadedOnDeactivate(t *testing.T) {
	h := newHarness(t, nil)

	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "x.go"), []byte("package helpers\nfunc One() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = h.bus.Emit("skill.loaded", events.SkillContent{SchemaVersion: events.SkillContentVersion, Name: "temp", BaseDir: skillDir})
	_ = h.bus.Emit("skill.deactivate", events.SkillRef{SchemaVersion: events.SkillRefVersion, Name: "temp"})

	script := `package main
import (
	"context"
	helpers "skills/temp"
)
func Run(ctx context.Context) (any, error) {
	return helpers.One(), nil
}
`
	res := h.runCode(script)
	if res.Error == "" {
		t.Fatalf("want import rejection after deactivate, got success")
	}
	if !strings.Contains(res.Error, "skills/temp") {
		t.Errorf("unexpected error: %q", res.Error)
	}
}

func TestPlugin_RunCodeNotExposedToItself(t *testing.T) {
	h := newHarness(t, nil)

	script := `package main
import (
	"context"
	"tools"
)
func Run(ctx context.Context) (any, error) {
	_, err := tools.RunCode(tools.RunCodeArgs{Script: ""})
	return nil, err
}
`
	res := h.runCode(script)
	if res.Error == "" {
		t.Fatalf("want compile error for self-reference, got success")
	}
}

// sanity: ensure our uses of engine.VetoablePayload compile against the shape
// the engine actually exports.
var _ = func() bool {
	_ = errors.New
	return true
}
