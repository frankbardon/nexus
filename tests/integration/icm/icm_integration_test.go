//go:build integration

// Package icm_test covers the nexus.workflows.icm plugin's bus-level wiring.
//
// These tests exercise the public plugin surface — Init, Ready, the bus
// subscriptions and emissions, and the icm_validate tool — through a real
// engine boot. The full orchestrator path (LLM dispatch, sub-agent loop,
// validators, fan-out, loops) is covered exhaustively by
// plugins/workflows/icm/runtime/orchestrator_test.go against fake
// dispatchers. We intentionally do NOT re-cover that ground here; the
// goal is to prove the plugin shell is wired correctly to the engine.
//
// Mock-mode caveat: the testharness/nexus.io.test mock_responses path
// vetoes before:llm.request, which is incompatible with delegate.Runtime's
// synchronous llm.request → llm.response round-trip (delegate sees the
// veto as a hard error). For the happy-path io.input flow we therefore
// assert on the deterministic events the plugin emits BEFORE any LLM call
// (plan.created, icm.run.started, tool.register) and accept that
// downstream stage dispatch surfaces "no LLM response" via the
// orchestrator's halt path. That still exercises the full subscription
// chain: io.input → handleInput → runWorkflow → emitPlanCreated →
// buildOrchestrator → orchestrator.Run → io.output.
package icm_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
	testio "github.com/frankbardon/nexus/plugins/io/test"
)

const validateToolName = "icm_validate"

// TestICM_Init_RejectsBadWorkspace boots an engine pointed at a workspace
// directory missing the required workspace.md. Init must fail loudly and
// engine.Boot must surface the error so embedders never silently run a
// half-loaded plugin.
func TestICM_Init_RejectsBadWorkspace(t *testing.T) {
	// Empty directory — no workspace.md, no stages/. The loader emits a
	// LoadErrors aggregate; ICM's Init wraps it as "icm: workspace load:".
	badWorkspace := t.TempDir()

	cfg := renderConfig(t, badWorkspace)

	eng, err := engine.NewFromBytes([]byte(cfg))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	allplugins.RegisterAll(eng.Registry)

	bootErr := eng.Boot(context.Background())
	if bootErr == nil {
		_ = eng.Stop(context.Background())
		t.Fatal("expected Boot to fail with workspace-load error, got nil")
	}
	// Surface the error to logs to aid future regressions.
	t.Logf("Boot error (expected): %v", bootErr)

	// Boot wraps as "plugin %q init failed: %w"; ICM wraps as "icm:
	// workspace load: %w". Probe both layers so a future tweak in either
	// spot surfaces here, not as a silent regression.
	msg := bootErr.Error()
	if !strings.Contains(msg, "nexus.workflows.icm") {
		t.Errorf("expected error to mention plugin id, got: %v", bootErr)
	}
	if !strings.Contains(msg, "workspace") {
		t.Errorf("expected error to mention workspace, got: %v", bootErr)
	}
}

// TestICM_Boot_HappyShell boots the engine against a valid two-stage
// workspace, drives a single io.input through nexus.io.test, and confirms
// the plugin's deterministic bus surface: capability presence,
// tool.register for icm_validate, and the plan.created +
// icm.run.started emissions that fire BEFORE any LLM dispatch. The
// orchestrator's halt path is expected (no LLM provider is active) and
// surfaces as an assistant io.output mentioning "halted".
func TestICM_Boot_HappyShell(t *testing.T) {
	ws := buildTwoStageWorkspace(t)
	cfgPath := writeConfig(t, ws)

	h := testharness.New(t, cfgPath,
		testharness.WithTimeout(5*time.Second),
		testharness.WithKeepSession(),
	)

	h.Run()

	// All three plugins must boot: io.test (driver), agent.postures
	// (capability provider), and the workflow plugin under test.
	h.AssertBooted("nexus.io.test", "nexus.workflows.icm", "nexus.agent.postures")

	// The plugin registers icm_validate at Ready. The test IO plugin's
	// wildcard collector captures the tool.register event.
	h.AssertEventEmitted("tool.register")
	names := collectToolRegisterNames(h.Events())
	if !containsString(names, validateToolName) {
		t.Errorf("expected %q in tool.register events, got: %v", validateToolName, names)
	}

	// io.input → handleInput → runWorkflow → emitPlanCreated. The plan's
	// step list mirrors the workspace's stages in execution order.
	h.AssertEventEmitted("plan.created")
	h.AssertEventEmitted("icm.run.started")

	// Plan should contain a step per stage (two stages in our fixture).
	if plan := findPlanCreated(h.Events()); plan == nil {
		t.Error("plan.created event payload not found or wrong type")
	} else if len(plan.Steps) != 2 {
		t.Errorf("expected 2 plan steps, got %d", len(plan.Steps))
	}

	// Without a real LLM provider, delegate.Runtime returns "no LLM
	// response..." and the orchestrator halts on the first stage's
	// default OnError=halt. The plugin's final io.output is an
	// assistant-role "Workflow halted: ..." narration. That confirms
	// the full bus chain reached io.output emission.
	h.AssertOutputContains("halted")
}

// TestICM_ValidateTool_OK invokes icm_validate via the bus against a
// valid workspace and asserts tool.result.Output == "ok".
func TestICM_ValidateTool_OK(t *testing.T) {
	ws := buildTwoStageWorkspace(t)
	cfgPath := writeConfig(t, ws)

	h := testharness.New(t, cfgPath, testharness.WithTimeout(5*time.Second))

	// Capture every icm_validate tool.result that lands on the bus.
	resultCh := make(chan events.ToolResult, 4)
	h.Bus().Subscribe("tool.result", func(ev engine.Event[any]) {
		if res, ok := ev.Payload.(events.ToolResult); ok && res.Name == validateToolName {
			select {
			case resultCh <- res:
			default:
			}
		}
	})

	// Wait for the plugin to register icm_validate before invoking it.
	// Subscribing on the test side guarantees we observe the registration
	// even though plugin Init order is otherwise opaque.
	registeredCh := make(chan struct{}, 1)
	h.Bus().Subscribe("tool.register", func(ev engine.Event[any]) {
		if def, ok := ev.Payload.(events.ToolDef); ok && def.Name == validateToolName {
			select {
			case registeredCh <- struct{}{}:
			default:
			}
		}
	})

	go func() {
		select {
		case <-registeredCh:
		case <-time.After(6 * time.Second):
			return
		}
		_ = h.Bus().Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "icm-validate-ok-1",
			Name:          validateToolName,
			Arguments:     map[string]any{"workspace": ws},
			TurnID:        "test-turn",
		})
	}()

	h.Run()

	res := waitForResult(t, resultCh, "icm-validate-ok-1", 8*time.Second)
	if res.Output != "ok" {
		t.Errorf("expected Output=ok, got %q (error=%q)", res.Output, res.Error)
	}
}

// TestICM_ValidateTool_Error invokes icm_validate against an empty
// directory and asserts the loader errors surface in tool.result.Output.
func TestICM_ValidateTool_Error(t *testing.T) {
	ws := buildTwoStageWorkspace(t)
	cfgPath := writeConfig(t, ws)
	badPath := t.TempDir()

	h := testharness.New(t, cfgPath, testharness.WithTimeout(5*time.Second))

	resultCh := make(chan events.ToolResult, 4)
	h.Bus().Subscribe("tool.result", func(ev engine.Event[any]) {
		if res, ok := ev.Payload.(events.ToolResult); ok && res.Name == validateToolName {
			select {
			case resultCh <- res:
			default:
			}
		}
	})

	registeredCh := make(chan struct{}, 1)
	h.Bus().Subscribe("tool.register", func(ev engine.Event[any]) {
		if def, ok := ev.Payload.(events.ToolDef); ok && def.Name == validateToolName {
			select {
			case registeredCh <- struct{}{}:
			default:
			}
		}
	})

	go func() {
		select {
		case <-registeredCh:
		case <-time.After(6 * time.Second):
			return
		}
		_ = h.Bus().Emit("tool.invoke", events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            "icm-validate-err-1",
			Name:          validateToolName,
			Arguments:     map[string]any{"workspace": badPath},
			TurnID:        "test-turn",
		})
	}()

	h.Run()

	res := waitForResult(t, resultCh, "icm-validate-err-1", 8*time.Second)
	if res.Output == "ok" {
		t.Fatalf("expected loader error output, got Output=ok")
	}
	if !strings.Contains(res.Output, "workspace") && !strings.Contains(res.Output, "stages") {
		t.Errorf("expected error to mention workspace/stages, got: %q", res.Output)
	}
}

// -- fixtures ---------------------------------------------------------------

// buildTwoStageWorkspace lays out a minimal but valid two-stage ICM
// workspace in a fresh temp dir and returns the absolute path. Stage 0
// emits a text artifact; stage 1 reads stage 0's output and emits its
// own. Neither stage declares predicates, schemas, loops, fan-outs, or
// human gates — the goal is the smallest workspace the loader accepts.
func buildTwoStageWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "workspace.md"),
		"# Integration test workspace\n\nA two-stage hermetic workspace for ICM integration tests.\n")
	writeFile(t, filepath.Join(root, "operator.md"),
		"# Operator\n\nYou are running stage {{.Stage.ID}} of workspace {{.Workspace.Name}}.\n")

	writeFile(t, filepath.Join(root, "stages", "01_outline", "contract.md"), `---
display: Draft outline
turns:
  policy: fixed
  max: 1
output:
  format: text
  filename: outline.txt
  persist: file_ref
---

Produce a brief outline.
`)

	writeFile(t, filepath.Join(root, "stages", "02_final", "contract.md"), `---
display: Final write-up
turns:
  policy: fixed
  max: 1
inputs:
  artifacts:
    - 01_outline/outline.txt
output:
  format: text
  filename: final.md
  persist: file_ref
---

Expand the outline into a final write-up.
`)

	return root
}

// writeFile creates parent dirs and writes content. Fatals on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeConfig renders the engine YAML for the happy-path tests and
// returns the absolute path. testharness.New tolerates absolute paths.
func writeConfig(t *testing.T, workspace string) string {
	t.Helper()
	cfg := renderConfig(t, workspace)
	dir := t.TempDir()
	path := filepath.Join(dir, "icm-test.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// renderConfig builds a hermetic engine config: log_level=warn, sessions
// and storage rooted under t.TempDir(), nexus.io.test as the IO surface,
// no LLM providers, one io.input to drive the workflow.
func renderConfig(t *testing.T, workspace string) string {
	t.Helper()
	sessionsRoot := t.TempDir()
	storageRoot := t.TempDir()
	return fmt.Sprintf(`core:
  log_level: warn
  tick_interval: 5s
  max_concurrent_events: 100
  sessions:
    root: %s
    retention: 30d
    id_format: datetime_short
  storage:
    root: %s

plugins:
  active:
    - nexus.io.test
    - nexus.agent.postures
    - nexus.workflows.icm

  nexus.io.test:
    inputs:
      - "go"
    input_delay: 100ms
    approval_mode: approve
    timeout: 3s

  nexus.agent.postures:
    scan_dirs: []

  nexus.workflows.icm:
    workspace: %s
`, sessionsRoot, storageRoot, workspace)
}

// -- helpers ----------------------------------------------------------------

// collectToolRegisterNames returns the Name of every tool.register
// payload in the captured event list. Used to assert specific tool
// registrations regardless of relative ordering.
func collectToolRegisterNames(evs []testio.CollectedEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Type != "tool.register" {
			continue
		}
		if def, ok := e.Payload.(events.ToolDef); ok {
			out = append(out, def.Name)
		}
	}
	return out
}

// findPlanCreated returns the first plan.created payload as a typed
// pointer, or nil when absent / not the expected type.
func findPlanCreated(evs []testio.CollectedEvent) *events.PlanResult {
	for _, e := range evs {
		if e.Type != "plan.created" {
			continue
		}
		if pr, ok := e.Payload.(events.PlanResult); ok {
			return &pr
		}
	}
	return nil
}

// containsString is a tiny membership check that avoids pulling in
// slices.Contains for a one-call site.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// waitForResult blocks until a tool.result with matching ID arrives or
// the deadline elapses. Fatals on timeout.
func waitForResult(t *testing.T, ch <-chan events.ToolResult, id string, timeout time.Duration) events.ToolResult {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case res := <-ch:
			if res.ID == id {
				return res
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for tool.result id=%s", id)
			return events.ToolResult{}
		}
	}
}
