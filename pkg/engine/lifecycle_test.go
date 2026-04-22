package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// testPlugin is a minimal engine.Plugin for lifecycle tests. Each field is
// captured so tests can verify that config propagation and activation order
// match expectations.
type testPlugin struct {
	id           string
	deps         []string
	requires     []Requirement
	capabilities []Capability
	observedCfg  map[string]any
	observedCaps map[string][]string
	initCalled   bool
	readyCalled  bool
	shutdownCall bool
}

func (t *testPlugin) ID() string             { return t.id }
func (t *testPlugin) Name() string           { return t.id }
func (t *testPlugin) Version() string        { return "test" }
func (t *testPlugin) Dependencies() []string { return t.deps }
func (t *testPlugin) Requires() []Requirement {
	return t.requires
}
func (t *testPlugin) Capabilities() []Capability { return t.capabilities }
func (t *testPlugin) Init(ctx PluginContext) error {
	t.initCalled = true
	t.observedCfg = ctx.Config
	t.observedCaps = ctx.Capabilities
	return nil
}
func (t *testPlugin) Ready() error                       { t.readyCalled = true; return nil }
func (t *testPlugin) Shutdown(_ context.Context) error   { t.shutdownCall = true; return nil }
func (t *testPlugin) Subscriptions() []EventSubscription { return nil }
func (t *testPlugin) Emissions() []string                { return nil }

// newTestLifecycle builds a lifecycle manager with the given plugin instances
// registered in a fresh PluginRegistry. Returns the manager, registry, a
// writable log buffer (for assertions on auto-activation logs), and a cleanup
// function. The order of supplied plugins is irrelevant — callers control
// the initial active list via config.Plugins.Active.
func newTestLifecycle(t *testing.T, active []string, plugins map[string]func() Plugin, configs map[string]map[string]any) (*LifecycleManager, *PluginRegistry, *strings.Builder) {
	t.Helper()
	reg := NewPluginRegistry()
	for id, f := range plugins {
		reg.Register(id, f)
	}
	cfg := &Config{
		Plugins: PluginsConfig{
			Active:  active,
			Configs: configs,
		},
	}
	buf := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := NewEventBus()
	lm := NewLifecycleManager(reg, bus, cfg, logger, nil, nil, nil, nil)
	return lm, reg, buf
}

// TestRequires_ActivatesMissingWithDefault covers the primary acceptance
// case: the user only lists "a" but "a" requires "b", so the engine must
// activate "b" with the declared Default config and log the auto-activation.
func TestRequires_ActivatesMissingWithDefault(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{
		{ID: "b", Default: map[string]any{"k": "from_default"}},
	}}
	b := &testPlugin{id: "b"}

	lm, _, buf := newTestLifecycle(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}

	if !b.initCalled {
		t.Fatal("expected b to be auto-activated and initialized")
	}
	if got := b.observedCfg["k"]; got != "from_default" {
		t.Errorf("b observed config[k] = %v, want \"from_default\"", got)
	}
	if origin := lm.provenance["b"]; origin.requiredBy != "a" || !origin.configFromDefault {
		t.Errorf("provenance[b] = %+v, want {requiredBy: a, configFromDefault: true}", origin)
	}
	if logs := buf.String(); !strings.Contains(logs, "auto-activating plugin") || !strings.Contains(logs, "required_by=a") {
		t.Errorf("expected auto-activation INFO log, got: %s", logs)
	}
}

// TestRequires_UserConfigWinsWhole covers the whole-object merge rule: when
// the user supplies any config for a required ID, Default must be discarded
// entirely — no field-level merge.
func TestRequires_UserConfigWinsWhole(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{
		{ID: "b", Default: map[string]any{"k1": "default_k1", "k2": "default_k2"}},
	}}
	b := &testPlugin{id: "b"}

	// User only supplies k1; default's k2 must NOT be merged in.
	lm, _, _ := newTestLifecycle(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
		},
		map[string]map[string]any{
			"b": {"k1": "user_k1"},
		},
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}

	if got := b.observedCfg["k1"]; got != "user_k1" {
		t.Errorf("b observed config[k1] = %v, want user_k1", got)
	}
	if _, present := b.observedCfg["k2"]; present {
		t.Error("k2 leaked from default into b's config — whole-object merge violated")
	}
	if lm.provenance["b"].configFromDefault {
		t.Error("configFromDefault should be false when user supplied config")
	}
}

// TestRequires_TransitiveChain verifies "a requires b; b requires c" walks
// the chain and activates c even though neither user nor a mentions it.
func TestRequires_TransitiveChain(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{{ID: "b"}}}
	b := &testPlugin{id: "b", requires: []Requirement{{ID: "c"}}}
	c := &testPlugin{id: "c"}

	lm, _, _ := newTestLifecycle(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
			"c": func() Plugin { return c },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if !c.initCalled {
		t.Fatal("c was not auto-activated transitively via b")
	}
	if lm.provenance["c"].requiredBy != "b" {
		t.Errorf("provenance[c].requiredBy = %q, want b", lm.provenance["c"].requiredBy)
	}
}

// TestRequires_OptionalMissingFactoryLogsWarn verifies Optional=true skips
// a missing factory with a WARN rather than failing boot.
func TestRequires_OptionalMissingFactoryLogsWarn(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{
		{ID: "unregistered", Optional: true},
	}}

	lm, _, buf := newTestLifecycle(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if logs := buf.String(); !strings.Contains(logs, "optional requirement skipped") {
		t.Errorf("expected optional-skip WARN log, got: %s", logs)
	}
}

// TestRequires_RequiredMissingFactoryFailsBoot verifies Optional=false (the
// default) fails boot with a clear error when the factory is unregistered.
func TestRequires_RequiredMissingFactoryFailsBoot(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{{ID: "unregistered"}}}

	lm, _, _ := newTestLifecycle(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
		},
		nil,
	)

	err := lm.Boot(context.Background())
	if err == nil {
		t.Fatal("expected boot to fail on missing required factory")
	}
	if !strings.Contains(err.Error(), "unregistered") || !strings.Contains(err.Error(), "no registered factory") {
		t.Errorf("error message did not call out the missing factory: %v", err)
	}
}

// TestRequires_AlreadyActiveSkipsDefault verifies that when the user has
// already listed the required ID in Active with their own config, the
// Requirement is a no-op — no duplicate activation, user config preserved.
func TestRequires_AlreadyActiveSkipsDefault(t *testing.T) {
	a := &testPlugin{id: "a", requires: []Requirement{
		{ID: "b", Default: map[string]any{"k": "default"}},
	}}
	b := &testPlugin{id: "b"}

	lm, _, _ := newTestLifecycle(t, []string{"a", "b"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
		},
		map[string]map[string]any{
			"b": {"k": "user"},
		},
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if got := b.observedCfg["k"]; got != "user" {
		t.Errorf("b observed config[k] = %v, want user", got)
	}
	if lm.provenance["b"].requiredBy != "" {
		t.Error("b provenance should show [user], not [auto]")
	}
}

// newCapLifecycle is a variant of newTestLifecycle that also accepts an
// explicit capabilities pin map so capability-resolution tests can exercise
// the Config.Capabilities path.
func newCapLifecycle(t *testing.T, active []string, capPins map[string]string, plugins map[string]func() Plugin, configs map[string]map[string]any) (*LifecycleManager, *PluginRegistry, *strings.Builder) {
	t.Helper()
	reg := NewPluginRegistry()
	for id, f := range plugins {
		reg.Register(id, f)
	}
	cfg := &Config{
		Capabilities: capPins,
		Plugins: PluginsConfig{
			Active:  active,
			Configs: configs,
		},
	}
	buf := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := NewEventBus()
	lm := NewLifecycleManager(reg, bus, cfg, logger, nil, nil, nil, nil)
	return lm, reg, buf
}

// TestCapability_ResolvedFromActiveList verifies that when a consumer's
// Requirement is capability-based and exactly one active plugin advertises
// the capability, no auto-activation happens — the active provider satisfies
// the requirement directly.
func TestCapability_ResolvedFromActiveList(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history"},
	}}
	provider := &testPlugin{id: "provider-a", capabilities: []Capability{
		{Name: "memory.history"},
	}}

	lm, _, buf := newCapLifecycle(t, []string{"consumer", "provider-a"}, nil,
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provider },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if got := lm.Capabilities()["memory.history"]; len(got) != 1 || got[0] != "provider-a" {
		t.Errorf("Capabilities[memory.history] = %v, want [provider-a]", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "capability satisfied") || !strings.Contains(logs, "source=active-list") {
		t.Errorf("expected capability-satisfied INFO log from active list, got: %s", logs)
	}
}

// TestCapability_AutoActivatesSoleRegisteredProvider covers the implicit
// fallback: only the consumer is in active, the capability has exactly one
// registered provider, so it gets auto-activated with source=auto-activated.
func TestCapability_AutoActivatesSoleRegisteredProvider(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history", Default: map[string]any{"k": "from_default"}},
	}}
	provider := &testPlugin{id: "provider-a", capabilities: []Capability{
		{Name: "memory.history"},
	}}

	lm, _, buf := newCapLifecycle(t, []string{"consumer"}, nil,
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provider },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if !provider.initCalled {
		t.Fatal("expected provider-a to be auto-activated via capability")
	}
	if got := provider.observedCfg["k"]; got != "from_default" {
		t.Errorf("provider-a observed config[k] = %v, want from_default", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "capability_source=auto-activated") {
		t.Errorf("expected auto-activated capability_source in INFO log, got: %s", logs)
	}
}

// TestCapability_AmbiguityWarnsAndPicksAlphabetically verifies that two
// registered providers for the same capability without an explicit pin emit
// a WARN naming both candidates and pick the alphabetically first ID.
func TestCapability_AmbiguityWarnsAndPicksAlphabetically(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history"},
	}}
	provA := &testPlugin{id: "provider-a", capabilities: []Capability{{Name: "memory.history"}}}
	provB := &testPlugin{id: "provider-b", capabilities: []Capability{{Name: "memory.history"}}}

	lm, _, buf := newCapLifecycle(t, []string{"consumer"}, nil,
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provA },
			"provider-b": func() Plugin { return provB },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if !provA.initCalled || provB.initCalled {
		t.Fatalf("expected only provider-a to be auto-activated (alphabetical tie-break); initCalled: a=%v b=%v", provA.initCalled, provB.initCalled)
	}
	logs := buf.String()
	if !strings.Contains(logs, "multiple candidate providers") {
		t.Errorf("expected ambiguity WARN, got: %s", logs)
	}
	if !strings.Contains(logs, "provider-a") || !strings.Contains(logs, "provider-b") {
		t.Errorf("ambiguity WARN should name every candidate, got: %s", logs)
	}
}

// TestCapability_ExplicitPinOverridesActiveList verifies Config.Capabilities
// takes precedence over the active-list order when multiple providers are
// eligible.
func TestCapability_ExplicitPinOverridesActiveList(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history"},
	}}
	provA := &testPlugin{id: "provider-a", capabilities: []Capability{{Name: "memory.history"}}}
	provB := &testPlugin{id: "provider-b", capabilities: []Capability{{Name: "memory.history"}}}

	// Both active, but pin points at B even though A comes first.
	lm, _, buf := newCapLifecycle(t,
		[]string{"consumer", "provider-a", "provider-b"},
		map[string]string{"memory.history": "provider-b"},
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provA },
			"provider-b": func() Plugin { return provB },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "source=explicit-config") {
		t.Errorf("expected explicit-config source in capability-resolved log, got: %s", logs)
	}
	// Still end up with both providers active (both in active list); the
	// pin is about which one *satisfies the requirement*, not which is the
	// only one allowed to run.
	caps := lm.Capabilities()["memory.history"]
	if len(caps) != 2 {
		t.Errorf("expected both providers in capability map, got: %v", caps)
	}
}

// TestCapability_MissingProviderFailsBoot verifies a Requirement.Capability
// with no registered provider fails boot when Optional=false.
func TestCapability_MissingProviderFailsBoot(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history"},
	}}

	lm, _, _ := newCapLifecycle(t, []string{"consumer"}, nil,
		map[string]func() Plugin{
			"consumer": func() Plugin { return consumer },
		},
		nil,
	)

	err := lm.Boot(context.Background())
	if err == nil {
		t.Fatal("expected boot to fail on missing capability provider")
	}
	if !strings.Contains(err.Error(), "memory.history") {
		t.Errorf("error message should name the missing capability, got: %v", err)
	}
}

// TestCapability_OptionalMissingLogsWarn verifies Optional=true with a
// missing capability provider logs a WARN and continues booting.
func TestCapability_OptionalMissingLogsWarn(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history", Optional: true},
	}}

	lm, _, buf := newCapLifecycle(t, []string{"consumer"}, nil,
		map[string]func() Plugin{
			"consumer": func() Plugin { return consumer },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if logs := buf.String(); !strings.Contains(logs, "optional capability requirement skipped") {
		t.Errorf("expected optional capability skip WARN, got: %s", logs)
	}
}

// TestCapability_MutuallyExclusiveWithID ensures a Requirement with both
// ID and Capability set fails boot loudly.
func TestCapability_MutuallyExclusiveWithID(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{ID: "provider-a", Capability: "memory.history"},
	}}

	lm, _, _ := newCapLifecycle(t, []string{"consumer"}, nil,
		map[string]func() Plugin{
			"consumer": func() Plugin { return consumer },
		},
		nil,
	)

	err := lm.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got: %v", err)
	}
}

// TestCapability_PluginContextReceivesMap verifies plugins see the resolved
// capability map in their PluginContext at Init.
func TestCapability_PluginContextReceivesMap(t *testing.T) {
	consumer := &testPlugin{id: "consumer"}
	provider := &testPlugin{id: "provider-a", capabilities: []Capability{
		{Name: "memory.history"},
	}}

	lm, _, _ := newCapLifecycle(t, []string{"consumer", "provider-a"}, nil,
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provider },
		},
		nil,
	)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("Boot failed: %v", err)
	}
	if got := consumer.observedCaps["memory.history"]; len(got) != 1 || got[0] != "provider-a" {
		t.Errorf("consumer ctx.Capabilities[memory.history] = %v, want [provider-a]", got)
	}
}
