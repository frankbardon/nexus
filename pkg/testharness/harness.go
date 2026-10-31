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
	"gopkg.in/yaml.v3"
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
	keepSession   bool // keep session dir unconditionally (used by tests that read post-run artifacts)
	failFast      bool // disable bus panic recovery so tests surface stack traces

	// configuredRoots keeps core.sessions.root and core.storage.root as the
	// config file states them instead of redirecting both into dataRoot.
	configuredRoots bool
	// dataRoot is the per-harness temp dir that holds the sessions root
	// (dataRoot/sessions) and the app/agent storage root (dataRoot/data).
	// Empty when configuredRoots is set.
	dataRoot string
	// rewrites transform the raw config bytes, in order, before the engine
	// is built from them. Set by WithConfigRewrite.
	rewrites []func([]byte) []byte
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

// WithKeepSession keeps the session directory unconditionally so tests can
// read post-run artifacts (the journal, plugin data dirs, metadata) after
// Run returns. The directory still lives under the harness's per-test temp
// root, which is removed when the test ends.
func WithKeepSession() Option {
	return func(h *Harness) {
		h.keepSession = true
	}
}

// WithConfiguredRoots keeps core.sessions.root and core.storage.root exactly
// as the config file sets them. Without it the harness redirects both into a
// per-test temp dir so no harness-driven test writes under ~/.nexus or shares
// state with another run. Use it only for a test that deliberately points
// the engine at a root it prepared itself and must find again.
func WithConfiguredRoots() Option {
	return func(h *Harness) {
		h.configuredRoots = true
	}
}

// WithConfigRewrite registers fn to transform the raw config file bytes
// before the engine is constructed from them (and before the sessions and
// storage roots are redirected). Rewrites run in the order given. It lets a
// test keep a checked-in config runnable by hand — a fixed loopback port, say
// — while substituting a per-test value, without writing a temp copy.
func WithConfigRewrite(fn func([]byte) []byte) Option {
	return func(h *Harness) {
		h.rewrites = append(h.rewrites, fn)
	}
}

// WithJudge sets a custom semantic judge for Tier 2 assertions.
func WithJudge(j Judge) Option {
	return func(h *Harness) {
		h.judge = j
	}
}

// WithFailFast disables the bus's panic-recovery wrapper so handler panics
// propagate with their original stack trace instead of being swallowed into
// a core.error event. Useful while iterating on a flaky test where the
// recovered emit obscures the real failure.
func WithFailFast() Option {
	return func(h *Harness) {
		h.failFast = true
	}
}

// New creates a test harness from a config file. The config should use
// nexus.io.test as its IO plugin.
//
// Unless WithConfiguredRoots is passed, the harness overrides the config's
// core.sessions.root and core.storage.root with directories under a fresh
// per-test temp dir, removed when the test ends (or retained, and logged,
// when WithRetainSession is set and the test failed).
func New(t *testing.T, configPath string, opts ...Option) *Harness {
	t.Helper()

	// Resolve config path relative to project root.
	absPath := configPath
	if !filepath.IsAbs(configPath) {
		root := findProjectRoot(t)
		absPath = filepath.Join(root, configPath)
	}

	h := &Harness{
		t:          t,
		configPath: absPath,
		timeout:    90 * time.Second,
	}

	for _, opt := range opts {
		opt(h)
	}

	var eng *engine.Engine
	eng, h.dataRoot = newEngine(t, absPath, h.configuredRoots, h.rewrites, &h.retainSession)
	h.eng = eng

	// Apply fail-fast on the bus before Boot so the very first dispatch in
	// startJournal subscriptions sees the toggled wrapper.
	if h.failFast {
		if setter, ok := eng.Bus.(interface{ SetFailFast(bool) }); ok {
			setter.SetFailFast(true)
		}
	}

	// Default judge: Anthropic Haiku if API key available.
	if h.judge == nil {
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			h.judge = NewAnthropicJudge(key)
		}
	}

	return h
}

// NewEngine builds an engine from configPath with every plugin registered
// (the registry cmd/nexus uses) and core.sessions.root / core.storage.root
// redirected into a per-test temp dir removed when the test ends, exactly as
// New does. It neither boots nor stops the engine: it is for tests whose
// driver is not the scripted nexus.io.test lifecycle (an inbound HTTP request
// against a serve transport, say) and so cannot use Harness.Run, but must
// still keep their state out of ~/.nexus. A relative configPath resolves
// against the project root.
//
// Only the options that shape engine construction take effect here
// (WithConfigRewrite, WithConfiguredRoots); the rest configure Harness.Run
// and are ignored.
func NewEngine(t *testing.T, configPath string, opts ...Option) *engine.Engine {
	t.Helper()
	absPath := configPath
	if !filepath.IsAbs(configPath) {
		absPath = filepath.Join(findProjectRoot(t), configPath)
	}
	h := &Harness{}
	for _, opt := range opts {
		opt(h)
	}
	eng, _ := newEngine(t, absPath, h.configuredRoots, h.rewrites, nil)
	return eng
}

// newEngine is the shared constructor behind New and NewEngine. Unless
// configuredRoots is set, it creates the per-test temp root, registers its
// cleanup, and builds the engine from the config with both roots rewritten
// into it, returning the temp root ("" under configuredRoots). retain, when
// non-nil, is read at cleanup: true plus a failed test keeps the dir.
// rewrites are applied to the raw config bytes first, in order.
//
// The rewrite happens on the YAML before the engine is constructed because
// the storage manager captures its root at construction; setting eng.Config
// afterwards would redirect sessions but not app/agent storage. engine.New is
// LoadConfig(file bytes) + construction, so NewFromBytes on the rewritten
// bytes is equivalent; ConfigPath is restored for config reloads.
func newEngine(t *testing.T, absPath string, configuredRoots bool, rewrites []func([]byte) []byte, retain *bool) (*engine.Engine, string) {
	t.Helper()
	fail := func(err error) {
		t.Fatalf("testharness: failed to create engine from %s: %v", absPath, err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		fail(fmt.Errorf("read config: %w", err))
	}
	for _, rw := range rewrites {
		data = rw(data)
	}

	if configuredRoots {
		eng, err := engine.NewFromBytes(data)
		if err != nil {
			fail(err)
		}
		eng.ConfigPath = absPath
		allplugins.RegisterAll(eng.Registry)
		return eng, ""
	}

	dir, err := os.MkdirTemp("", "nexus-testharness-")
	if err != nil {
		fail(fmt.Errorf("create temp root: %w", err))
	}
	t.Cleanup(func() {
		if retain != nil && *retain && t.Failed() {
			t.Logf("testharness: retaining temp root: %s", dir)
			return
		}
		os.RemoveAll(dir)
	})

	data, err = overrideRoots(data, filepath.Join(dir, "sessions"), filepath.Join(dir, "data"))
	if err != nil {
		fail(fmt.Errorf("override roots: %w", err))
	}
	eng, err := engine.NewFromBytes(data)
	if err != nil {
		fail(err)
	}
	eng.ConfigPath = absPath
	allplugins.RegisterAll(eng.Registry)
	return eng, dir
}

// overrideRoots sets core.sessions.root and core.storage.root in a YAML
// config, creating the core/sessions/storage mappings when absent and
// leaving every other key untouched.
func overrideRoots(in []byte, sessionsRoot, storageRoot string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml root is not a mapping")
	}
	core := mappingChild(top, "core")
	setScalar(mappingChild(core, "sessions"), "root", sessionsRoot)
	setScalar(mappingChild(core, "storage"), "root", storageRoot)
	return yaml.Marshal(&doc)
}

// mappingChild returns the mapping stored under key in parent, creating it
// (or replacing a non-mapping value) as needed.
func mappingChild(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Kind == yaml.ScalarNode && parent.Content[i].Value == key {
			v := parent.Content[i+1]
			if v.Kind != yaml.MappingNode {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setScalar sets key to a string scalar value in a mapping node.
func setScalar(m *yaml.Node, key, value string) {
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
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

	// Clean up session dir unless retaining for debug or keeping for test
	// reads. WithKeepSession is unconditional; WithRetainSession only
	// kicks in when the test failed.
	keep := h.keepSession || (h.retainSession && h.t.Failed())
	if !keep {
		if h.eng.Session != nil {
			os.RemoveAll(h.eng.Session.RootDir)
		}
	} else if h.eng.Session != nil {
		h.t.Logf("testharness: retaining session dir: %s", h.eng.Session.RootDir)
	}
}

// TempRoot returns the per-test temp dir the harness redirected
// core.sessions.root (TempRoot()/sessions) and core.storage.root
// (TempRoot()/data) into. Empty under WithConfiguredRoots.
func (h *Harness) TempRoot() string {
	return h.dataRoot
}

// SessionDir returns the absolute path of the session workspace created by
// the harness. Empty before Run; remains valid after Run when paired with
// WithKeepSession.
func (h *Harness) SessionDir() string {
	if h.eng == nil || h.eng.Session == nil {
		return ""
	}
	return h.eng.Session.RootDir
}

// Events returns all collected events from the test run.
func (h *Harness) Events() []testio.CollectedEvent {
	h.t.Helper()
	h.requireRan()
	return h.testPlugin.Collected()
}

// Bus returns the engine's event bus. Use this before Run() to subscribe
// custom handlers for events the test IO plugin's wildcard collector does
// not observe — notably `before:*` vetoable events, which the bus dispatches
// only to explicitly-subscribed handlers.
func (h *Harness) Bus() engine.EventBus {
	h.t.Helper()
	if h.eng == nil {
		h.t.Fatal("testharness: engine not initialized")
	}
	return h.eng.Bus
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
