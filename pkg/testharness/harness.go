// Package testharness provides integration test helpers for Nexus.
// It wraps engine setup, plugin registration, and event assertions
// for use in Go test files.
//
//	h := testharness.New(t, "configs/test-minimal.yaml")
//	h.Run()
//	h.AssertNoSystemOutput()
//	h.AssertEventEmitted("io.output")
package testharness

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
	testio "github.com/frankbardon/nexus/plugins/io/test"
)

const testPluginID = "nexus.io.test"

// Harness wraps a Nexus engine for integration testing.
type Harness struct {
	t          *testing.T
	configPath string
	eng        *engine.Engine
	testPlugin *testio.Plugin
	judge      Judge

	timeout       time.Duration
	retainSession bool // keep session dir on failure for debugging
}

// Option configures the test harness.
type Option func(*Harness)

// WithTimeout overrides the default session timeout.
func WithTimeout(d time.Duration) Option {
	return func(h *Harness) {
		h.timeout = d
	}
}

// WithRetainSession keeps the session directory on test failure for debugging.
func WithRetainSession() Option {
	return func(h *Harness) {
		h.retainSession = true
	}
}

// WithJudge sets a custom semantic judge for Tier 2 assertions.
func WithJudge(j Judge) Option {
	return func(h *Harness) {
		h.judge = j
	}
}

// New creates a test harness from a config file. The config should use
// nexus.io.test as its IO plugin.
func New(t *testing.T, configPath string, opts ...Option) *Harness {
	t.Helper()

	// Resolve config path relative to project root.
	absPath := configPath
	if !filepath.IsAbs(configPath) {
		root := findProjectRoot(t)
		absPath = filepath.Join(root, configPath)
	}

	eng, err := engine.New(absPath)
	if err != nil {
		t.Fatalf("testharness: failed to create engine from %s: %v", configPath, err)
	}

	allplugins.RegisterAll(eng.Registry)

	h := &Harness{
		t:          t,
		configPath: absPath,
		eng:        eng,
		timeout:    90 * time.Second,
	}

	for _, opt := range opts {
		opt(h)
	}

	// Default judge: Anthropic Haiku if API key available.
	if h.judge == nil {
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			h.judge = NewAnthropicJudge(key)
		}
	}

	return h
}

// Run boots the engine, waits for the test IO plugin to signal completion
// (or timeout), then stops the engine. Call assertion methods after Run returns.
func (h *Harness) Run() {
	h.t.Helper()

	ctx := context.Background()

	if err := h.eng.Boot(ctx); err != nil {
		h.t.Fatalf("testharness: engine boot failed: %v", err)
	}

	// Find the test IO plugin instance from the lifecycle manager.
	h.testPlugin = h.findTestPlugin()
	if h.testPlugin == nil {
		h.eng.Stop(ctx)
		h.t.Fatalf("testharness: nexus.io.test plugin not found — config must include it in plugins.active")
	}

	// Wait for test completion or timeout.
	timer := time.NewTimer(h.timeout)
	defer timer.Stop()

	select {
	case <-h.testPlugin.Done():
	case <-h.eng.SessionEnded():
	case <-timer.C:
		h.t.Logf("testharness: timeout after %s", h.timeout)
	}

	if err := h.eng.Stop(ctx); err != nil {
		h.t.Logf("testharness: engine stop error: %v", err)
	}

	// Clean up session dir unless retaining for debug.
	if !h.retainSession || !h.t.Failed() {
		if h.eng.Session != nil {
			os.RemoveAll(h.eng.Session.RootDir)
		}
	} else if h.eng.Session != nil {
		h.t.Logf("testharness: retaining session dir: %s", h.eng.Session.RootDir)
	}
}

// Events returns all collected events from the test run.
func (h *Harness) Events() []testio.CollectedEvent {
	h.t.Helper()
	h.requireRan()
	return h.testPlugin.Collected()
}

// -- Tier 1: Deterministic assertions ----------------------------------------

// AssertEventEmitted fails if no event of the given type was collected.
func (h *Harness) AssertEventEmitted(eventType string) {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == eventType {
			return
		}
	}
	h.t.Errorf("expected event %q to be emitted, but it was not", eventType)
}

// AssertEventNotEmitted fails if any event of the given type was collected.
func (h *Harness) AssertEventNotEmitted(eventType string) {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == eventType {
			h.t.Errorf("expected event %q NOT to be emitted, but it was", eventType)
			return
		}
	}
}

// AssertEventCount fails if the count of events matching eventType is outside [min, max].
func (h *Harness) AssertEventCount(eventType string, min, max int) {
	h.t.Helper()
	h.requireRan()
	count := 0
	for _, e := range h.testPlugin.Collected() {
		if e.Type == eventType {
			count++
		}
	}
	if count < min || count > max {
		h.t.Errorf("expected %d-%d %q events, got %d", min, max, eventType, count)
	}
}

// AssertSystemOutputContains fails if no io.output event with role "system"
// contains the given substring. Gates emit system-role output when they veto.
func (h *Harness) AssertSystemOutputContains(substring string) {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok {
				if out.Role == "system" && strings.Contains(out.Content, substring) {
					return
				}
			}
		}
	}
	// Diagnostic: show what io.output events were actually collected.
	var diag []string
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok {
				diag = append(diag, fmt.Sprintf("  role=%q content=%s", out.Role, truncate(out.Content, 120)))
			} else {
				diag = append(diag, fmt.Sprintf("  (type assertion failed: %T)", e.Payload))
			}
		}
	}
	if len(diag) == 0 {
		h.t.Errorf("expected system output containing %q, but no io.output events collected at all", substring)
	} else {
		h.t.Errorf("expected system output containing %q, but none matched.\n  collected io.output events:\n%s",
			substring, strings.Join(diag, "\n"))
	}
}

// AssertNoSystemOutput fails if any io.output event with role "system" was emitted.
// System-role outputs typically indicate gate vetoes or errors.
func (h *Harness) AssertNoSystemOutput() {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok {
				if out.Role == "system" {
					h.t.Errorf("expected no system output, but found: %s", truncate(out.Content, 200))
					return
				}
			}
		}
	}
}

// AssertOutputContains fails if no io.output event with role "assistant" contains
// the given substring.
func (h *Harness) AssertOutputContains(substring string) {
	h.t.Helper()
	h.requireRan()
	for _, content := range h.assistantOutputs() {
		if strings.Contains(content, substring) {
			return
		}
	}
	h.t.Errorf("expected assistant output containing %q, but none found", substring)
}

// AssertOutputNotContains fails if any assistant output contains the given substring.
func (h *Harness) AssertOutputNotContains(substring string) {
	h.t.Helper()
	h.requireRan()
	for _, content := range h.assistantOutputs() {
		if strings.Contains(content, substring) {
			h.t.Errorf("expected assistant output NOT containing %q, but found it", substring)
			return
		}
	}
}

// AssertToolCalled fails if no tool.invoke event matches the given tool name.
func (h *Harness) AssertToolCalled(toolName string) {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "tool.invoke" {
			if call, ok := e.Payload.(events.ToolCall); ok {
				if call.Name == toolName {
					return
				}
			}
		}
	}
	h.t.Errorf("expected tool %q to be called, but it was not", toolName)
}

// AssertToolNotCalled fails if any tool.invoke event matches the given tool name.
func (h *Harness) AssertToolNotCalled(toolName string) {
	h.t.Helper()
	h.requireRan()
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "tool.invoke" {
			if call, ok := e.Payload.(events.ToolCall); ok {
				if call.Name == toolName {
					h.t.Errorf("expected tool %q NOT to be called, but it was", toolName)
					return
				}
			}
		}
	}
}

// AssertSessionArtifact fails if the given relative path does not exist in the session dir.
func (h *Harness) AssertSessionArtifact(relPath string) {
	h.t.Helper()
	h.requireRan()
	if h.eng.Session == nil {
		h.t.Errorf("no session workspace available")
		return
	}
	fullPath := filepath.Join(h.eng.Session.RootDir, relPath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		h.t.Errorf("expected session artifact %q, but it does not exist", relPath)
	}
}

// AssertBooted fails if any of the given plugin IDs were not initialized.
// Checks against the lifecycle manager's active plugin list.
func (h *Harness) AssertBooted(pluginIDs ...string) {
	h.t.Helper()
	h.requireRan()
	if h.eng.Lifecycle == nil {
		h.t.Errorf("no lifecycle manager available")
		return
	}
	booted := make(map[string]bool)
	for _, p := range h.eng.Lifecycle.Plugins() {
		booted[p.ID()] = true
	}
	for _, id := range pluginIDs {
		if !booted[id] {
			h.t.Errorf("expected plugin %q to be booted, but it was not", id)
		}
	}
}

// -- Tier 2: Semantic assertions ---------------------------------------------

// AssertOutputSemantic uses the configured LLM judge to evaluate whether
// assistant output satisfies the given criteria. Skips if no judge configured.
func (h *Harness) AssertOutputSemantic(criteria string) {
	h.t.Helper()
	h.requireRan()

	if h.judge == nil {
		h.t.Skip("testharness: no semantic judge configured (set ANTHROPIC_API_KEY)")
		return
	}

	outputs := h.assistantOutputs()
	if len(outputs) == 0 {
		h.t.Errorf("semantic assertion %q: no assistant output to evaluate", criteria)
		return
	}

	combined := strings.Join(outputs, "\n---\n")
	result, err := h.judge.Evaluate(context.Background(), combined, criteria)
	if err != nil {
		h.t.Errorf("semantic assertion %q: judge error: %v", criteria, err)
		return
	}
	if !result.Pass {
		h.t.Errorf("semantic assertion failed: %q\n  reason: %s\n  output: %s",
			criteria, result.Reason, truncate(combined, 500))
	}
}

// -- helpers -----------------------------------------------------------------

func (h *Harness) requireRan() {
	h.t.Helper()
	if h.testPlugin == nil {
		h.t.Fatal("testharness: must call Run() before assertions")
	}
}

func (h *Harness) assistantOutputs() []string {
	var outputs []string
	for _, e := range h.testPlugin.Collected() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok {
				if out.Role == "assistant" {
					outputs = append(outputs, out.Content)
				}
			}
		}
	}
	return outputs
}

func (h *Harness) findTestPlugin() *testio.Plugin {
	if h.eng.Lifecycle == nil {
		return nil
	}
	for _, p := range h.eng.Lifecycle.Plugins() {
		if p.ID() == testPluginID {
			if tp, ok := p.(*testio.Plugin); ok {
				return tp
			}
		}
	}
	return nil
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("testharness: cannot get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("testharness: cannot find project root (no go.mod)")
		}
		dir = parent
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + fmt.Sprintf("... (%d chars total)", len(s))
}
