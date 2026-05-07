// Package contract provides a lightweight unit-level test harness for
// asserting Nexus plugin event contracts (Subscriptions / Emissions).
//
// It deliberately lives outside pkg/testharness so plugin packages can
// import it without dragging in pkg/engine/allplugins (which would form
// an import cycle: plugin → harness → allplugins → plugin).
package contract

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/host" // register default backend
	"github.com/frankbardon/nexus/pkg/engine/storage"
)

// CapturedEvent records a single event observed on the bus during a contract
// test. Origin distinguishes events injected by the test from events emitted
// by the plugin under test.
type CapturedEvent struct {
	Type    string
	Source  string
	Payload any
	Origin  EventOrigin
}

// EventOrigin tags whether an event arrived via test injection or plugin emit.
type EventOrigin int

const (
	OriginUnknown EventOrigin = iota
	OriginInject              // emitted by ContractHarness.Inject
	OriginPlugin              // emitted by the plugin or any handler reacting to an inject
)

// ContractHarness runs a single Nexus plugin in isolation against a real
// engine.Bus and a minimal PluginContext, capturing every emitted event so
// tests can assert against the plugin's declared event contract.
//
// Use NewContract to construct, Inject to drive scripted events, and the
// Assert* methods to check what the plugin did. Shutdown is registered with
// t.Cleanup automatically.
type ContractHarness struct {
	t      *testing.T
	plugin engine.Plugin
	bus    engine.EventBus

	mu           sync.Mutex
	captured     []CapturedEvent
	injectedIDs  map[string]struct{}
	storage      *storage.Manager
	sessionDir   string
	allowJournal bool
	withSession  bool
}

// ContractOption configures a ContractHarness.
type ContractOption func(*contractConfig)

type contractConfig struct {
	pluginID    string
	pluginCfg   map[string]any
	withSession bool
	logger      *slog.Logger
}

// WithPluginID overrides the harness-supplied plugin ID. Useful when a
// plugin uses InstanceID-aware naming. Defaults to plugin.ID().
func WithPluginID(id string) ContractOption {
	return func(c *contractConfig) { c.pluginID = id }
}

// WithPluginConfig provides the YAML-derived config map the plugin would
// normally receive from the engine.
func WithPluginConfig(cfg map[string]any) ContractOption {
	return func(c *contractConfig) { c.pluginCfg = cfg }
}

// WithSession boots the harness with a real SessionWorkspace pointed at a
// per-test temp dir. Enables plugins that touch ctx.Session, ctx.DataDir, or
// ScopeSession storage. Off by default to keep tests fast.
func WithSession() ContractOption {
	return func(c *contractConfig) { c.withSession = true }
}

// WithLogger overrides the default discard logger.
func WithLogger(l *slog.Logger) ContractOption {
	return func(c *contractConfig) { c.logger = l }
}

// NewContract constructs a ContractHarness, calls Init and Ready on the
// plugin, and registers a cleanup that drains the bus and calls Shutdown.
func NewContract(t *testing.T, factory engine.PluginFactory, opts ...ContractOption) *ContractHarness {
	t.Helper()

	cfg := &contractConfig{
		pluginCfg: map[string]any{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(cfg)
	}

	plugin := factory()
	if plugin == nil {
		t.Fatal("contract harness: factory returned nil plugin")
	}
	pluginID := cfg.pluginID
	if pluginID == "" {
		pluginID = plugin.ID()
	}

	h := &ContractHarness{
		t:           t,
		plugin:      plugin,
		bus:         engine.NewEventBus(),
		injectedIDs: map[string]struct{}{},
		withSession: cfg.withSession,
	}

	// Capture every event with origin tagging. Subscribe BEFORE Init so the
	// plugin's own handler-side emissions during Init (rare but possible)
	// are observed.
	h.bus.SubscribeAll(func(ev engine.Event[any]) {
		h.mu.Lock()
		defer h.mu.Unlock()
		origin := OriginPlugin
		if _, ok := h.injectedIDs[ev.ID]; ok {
			origin = OriginInject
		}
		h.captured = append(h.captured, CapturedEvent{
			Type:    ev.Type,
			Source:  ev.Source,
			Payload: ev.Payload,
			Origin:  origin,
		})
	})

	// Build per-test temp dirs. Storage manager lives under root; sandbox
	// uses default host backend so ctx.Sandbox is non-nil.
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatalf("contract harness: mkdir data root: %v", err)
	}

	var sessionDir string
	var session *engine.SessionWorkspace
	if cfg.withSession {
		var err error
		session, err = engine.NewSessionWorkspace(filepath.Join(root, "sessions"), h.bus)
		if err != nil {
			t.Fatalf("contract harness: new session workspace: %v", err)
		}
		sessionDir = session.RootDir
	}
	h.sessionDir = sessionDir

	h.storage = storage.NewManager(dataRoot, "", func() string { return sessionDir }, nil)

	dataDir := filepath.Join(dataRoot, "plugins", pluginID)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("contract harness: mkdir plugin data dir: %v", err)
	}

	sb, err := sandbox.New(sandbox.BackendHost, nil)
	if err != nil {
		t.Fatalf("contract harness: build sandbox: %v", err)
	}

	pluginCtx := engine.PluginContext{
		Config:       cfg.pluginCfg,
		Bus:          h.bus,
		Logger:       cfg.logger,
		PluginID:     pluginID,
		DataDir:      dataDir,
		AppDataDir:   dataDir,
		AgentDataDir: dataDir,
		Storage: func(scope storage.Scope) (storage.Storage, error) {
			return h.storage.Open(scope, pluginID)
		},
		Session:      session,
		Capabilities: map[string][]string{},
		Replay:       engine.NewReplayState(),
		Sandbox:      sb,
	}

	if err := plugin.Init(pluginCtx); err != nil {
		t.Fatalf("contract harness: plugin Init: %v", err)
	}
	if err := plugin.Ready(); err != nil {
		t.Fatalf("contract harness: plugin Ready: %v", err)
	}

	t.Cleanup(func() {
		_ = h.bus.Drain(context.Background())
		_ = plugin.Shutdown(context.Background())
		_ = h.storage.Close()
	})

	return h
}

// Plugin returns the plugin under test.
func (h *ContractHarness) Plugin() engine.Plugin { return h.plugin }

// Bus returns the harness bus so tests can subscribe directly when they
// need to observe vetoable events or set up extra handlers.
func (h *ContractHarness) Bus() engine.EventBus { return h.bus }

// SessionDir returns the harness's session workspace root, or empty when
// the harness was constructed without WithSession.
func (h *ContractHarness) SessionDir() string { return h.sessionDir }

// Inject emits an event on the harness bus and records its ID so the
// captured-event stream can distinguish it from plugin emissions.
func (h *ContractHarness) Inject(eventType string, payload any) {
	h.t.Helper()
	id := engine.GenerateID()
	h.mu.Lock()
	h.injectedIDs[id] = struct{}{}
	h.mu.Unlock()

	ev := engine.Event[any]{
		Type:    eventType,
		ID:      id,
		Source:  "testharness.inject",
		Payload: payload,
	}
	if err := h.bus.EmitEvent(ev); err != nil {
		h.t.Fatalf("contract harness: inject %q: %v", eventType, err)
	}
}

// InjectVetoable dispatches a before:* event and returns the resulting
// VetoResult so tests can assert that a gate vetoed (or didn't) and read
// the reason.
func (h *ContractHarness) InjectVetoable(eventType string, payload any) engine.VetoResult {
	h.t.Helper()
	id := engine.GenerateID()
	h.mu.Lock()
	h.injectedIDs[id] = struct{}{}
	h.mu.Unlock()
	// Note: EmitVetoable does not let us thread a custom event ID; the
	// captured-stream filter will not match this inject. That's acceptable —
	// the vetoable event is wrapped in a VetoablePayload anyway and the
	// plugin's handler operates on it inline rather than by emitting.
	res, err := h.bus.EmitVetoable(eventType, payload)
	if err != nil {
		h.t.Fatalf("contract harness: inject vetoable %q: %v", eventType, err)
	}
	return res
}

// Captured returns a copy of all events observed on the bus, in order.
func (h *ContractHarness) Captured() []CapturedEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CapturedEvent, len(h.captured))
	copy(out, h.captured)
	return out
}

// PluginEmissions returns only the events the plugin (or downstream
// handlers) emitted, filtering out test injects.
func (h *ContractHarness) PluginEmissions() []CapturedEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CapturedEvent, 0, len(h.captured))
	for _, e := range h.captured {
		if e.Origin == OriginPlugin {
			out = append(out, e)
		}
	}
	return out
}

// AssertEmitted fails when no plugin-origin event of the given type was
// captured.
func (h *ContractHarness) AssertEmitted(eventType string) {
	h.t.Helper()
	for _, e := range h.PluginEmissions() {
		if e.Type == eventType {
			return
		}
	}
	h.t.Errorf("expected emission %q from plugin %q; observed %s",
		eventType, h.plugin.ID(), h.summary())
}

// AssertNotEmitted fails when any plugin-origin event of the given type
// was captured.
func (h *ContractHarness) AssertNotEmitted(eventType string) {
	h.t.Helper()
	for _, e := range h.PluginEmissions() {
		if e.Type == eventType {
			h.t.Errorf("expected NO emission %q from plugin %q; observed %s",
				eventType, h.plugin.ID(), h.summary())
			return
		}
	}
}

// AssertEmittedInOrder fails when the named event types do not appear in
// the captured plugin-origin stream in the given relative order. Other
// emissions between them are ignored.
func (h *ContractHarness) AssertEmittedInOrder(types ...string) {
	h.t.Helper()
	if len(types) == 0 {
		return
	}
	idx := 0
	for _, e := range h.PluginEmissions() {
		if e.Type == types[idx] {
			idx++
			if idx == len(types) {
				return
			}
		}
	}
	h.t.Errorf("expected order %v from plugin %q; observed %s",
		types, h.plugin.ID(), h.summary())
}

// AssertNoUndeclaredEmissions fails when the plugin emitted an event type
// not listed in its declared Emissions() set. before:* events the plugin
// dispatches as part of veto-protocol handling are exempt because the
// declaration list typically only carries non-vetoable types.
func (h *ContractHarness) AssertNoUndeclaredEmissions() {
	h.t.Helper()
	declared := map[string]bool{}
	for _, t := range h.plugin.Emissions() {
		declared[t] = true
	}
	for _, e := range h.PluginEmissions() {
		if e.Source == "" {
			// Bus-level emits without an explicit Source can be system
			// events (e.g. core.error) the plugin did not directly emit.
			continue
		}
		if e.Source != h.plugin.ID() && e.Source != strings.SplitN(h.plugin.ID(), "/", 2)[0] {
			continue
		}
		if !declared[e.Type] {
			h.t.Errorf("plugin %q emitted undeclared event %q (declared: %v)",
				h.plugin.ID(), e.Type, h.plugin.Emissions())
		}
	}
}

// AssertSubscribesTo fails when the plugin's declared Subscriptions does
// not include every type in the given list. Compares against the static
// declaration, not runtime behavior — pair with Inject + AssertEmitted to
// verify a subscription actually fires.
func (h *ContractHarness) AssertSubscribesTo(eventTypes ...string) {
	h.t.Helper()
	subs := h.plugin.Subscriptions()
	have := make([]string, 0, len(subs))
	for _, s := range subs {
		have = append(have, s.EventType)
	}
	for _, want := range eventTypes {
		if !slices.Contains(have, want) {
			h.t.Errorf("plugin %q does not declare subscription to %q (declared: %v)",
				h.plugin.ID(), want, have)
		}
	}
}

// summary renders the captured stream as a one-line diagnostic.
func (h *ContractHarness) summary() string {
	emissions := h.PluginEmissions()
	if len(emissions) == 0 {
		return "(no plugin emissions captured)"
	}
	var sb strings.Builder
	sb.WriteString("plugin emissions: [")
	for i, e := range emissions {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s", e.Type)
	}
	sb.WriteString("]")
	return sb.String()
}
