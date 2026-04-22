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
	observedCfg  map[string]any
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
func (t *testPlugin) Init(ctx PluginContext) error {
	t.initCalled = true
	t.observedCfg = ctx.Config
	return nil
}
func (t *testPlugin) Ready() error                    { t.readyCalled = true; return nil }
func (t *testPlugin) Shutdown(_ context.Context) error { t.shutdownCall = true; return nil }
func (t *testPlugin) Subscriptions() []EventSubscription { return nil }
func (t *testPlugin) Emissions() []string               { return nil }

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
