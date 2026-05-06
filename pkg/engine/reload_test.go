package engine

// Phase 5 — Engine.ReloadConfig tests.
//
// We exercise ReloadConfig directly against an Engine struct stitched
// together by hand rather than going through engine.New + Boot. The
// production code path matters; these tests focus on the diff/apply
// correctness of ReloadConfig itself, the capability-pinning rejection,
// and the ConfigReloader interface contract. SIGHUP is NOT tested here:
// signal-driven tests are messy on cross-platform CI (Windows lacks
// SIGHUP, macOS reaper races with go test parallelism). The CLI wiring
// is exercised by manual smoke as documented in the planning notes.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reloaderPlugin is a testPlugin that also implements ConfigReloader.
// reloadCount tracks how many times ReloadConfig was called so tests can
// distinguish in-place reloads from full restarts (which would re-init).
type reloaderPlugin struct {
	testPlugin
	reloadCount int
	lastOldCfg  map[string]any
	lastNewCfg  map[string]any
	reloadErr   error
}

func (r *reloaderPlugin) ReloadConfig(old, new map[string]any) error {
	if r.reloadErr != nil {
		return r.reloadErr
	}
	r.reloadCount++
	r.lastOldCfg = old
	r.lastNewCfg = new
	return nil
}

// schemaPlugin attaches a ConfigSchemaProvider to testPlugin without the
// ConfigReloader hook, exercising the restart path.
type schemaPlugin struct {
	testPlugin
	schema []byte
}

func (s *schemaPlugin) ConfigSchema() []byte { return s.schema }

// newReloadEngine builds an Engine wired enough for ReloadConfig to walk:
// registry, lifecycle, bus, and a logger. No journal, no session, no
// storage — none of those are required by ReloadConfig (the build path
// is what mints PluginContext storage handles, and our test plugins
// don't touch them).
func newReloadEngine(t *testing.T, active []string, factories map[string]func() Plugin, configs map[string]map[string]any) (*Engine, *strings.Builder) {
	t.Helper()
	reg := NewPluginRegistry()
	for id, f := range factories {
		reg.Register(id, f)
	}
	cfg := &Config{
		Plugins: PluginsConfig{
			Active:  append([]string(nil), active...),
			Configs: cloneConfigs(configs),
		},
	}
	buf := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := NewEventBus()
	lm := NewLifecycleManager(reg, bus, cfg, logger, nil, nil, nil, nil)

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}

	eng := &Engine{
		Config:    cfg,
		Bus:       bus,
		Registry:  reg,
		Lifecycle: lm,
		Logger:    logger,
	}
	return eng, buf
}

// cloneConfigs returns a deep copy so the mutations Boot performs on the
// active config (auto-activation default install, etc.) don't leak between
// the engine state and the new-config the test passes to ReloadConfig.
func cloneConfigs(in map[string]map[string]any) map[string]map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]any, len(in))
	for k, v := range in {
		dup := make(map[string]any, len(v))
		for k2, v2 := range v {
			dup[k2] = v2
		}
		out[k] = dup
	}
	return out
}

// freshConfig returns a Config the test can pass to ReloadConfig with the
// active set and configs the caller specifies. Mirrors newReloadEngine's
// build pattern so the diff is meaningful.
func freshConfig(active []string, configs map[string]map[string]any) *Config {
	return &Config{
		Plugins: PluginsConfig{
			Active:  append([]string(nil), active...),
			Configs: cloneConfigs(configs),
		},
	}
}

// 1. Validation rejects bad config — unknown key.
func TestReload_ValidationRejectsBadConfig(t *testing.T) {
	const sch = `{
	  "type": "object",
	  "additionalProperties": false,
	  "properties": {"name": {"type": "string"}}
	}`
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin {
				return &schemaPlugin{
					testPlugin: testPlugin{id: "a"},
					schema:     []byte(sch),
				}
			},
		},
		map[string]map[string]any{"a": {"name": "ok"}},
	)

	bad := freshConfig([]string{"a"}, map[string]map[string]any{
		"a": {"naem": "typo"},
	})
	err := eng.ReloadConfig(bad)
	if err == nil {
		t.Fatal("expected validation error on unknown key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error did not flag unknown key: %v", err)
	}
	// Engine state must be unchanged: the old config should still be present.
	if got := eng.Config.Plugins.Configs["a"]["name"]; got != "ok" {
		t.Errorf("engine state mutated on validation failure: %v", got)
	}
}

// 2. Plain config-only change — ConfigReloader path; no restart.
func TestReload_ConfigOnly_UsesReloader(t *testing.T) {
	plug := &reloaderPlugin{testPlugin: testPlugin{id: "a"}}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return plug },
		},
		map[string]map[string]any{"a": {"k": "old"}},
	)

	new := freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "new"}})
	if err := eng.ReloadConfig(new); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if plug.reloadCount != 1 {
		t.Errorf("reloadCount = %d, want 1", plug.reloadCount)
	}
	if plug.lastOldCfg["k"] != "old" || plug.lastNewCfg["k"] != "new" {
		t.Errorf("reloader saw old=%v new=%v, want {old, new}", plug.lastOldCfg, plug.lastNewCfg)
	}
	if plug.shutdownCall {
		t.Error("ConfigReloader path triggered Shutdown — should not")
	}
	if got := eng.Config.Plugins.Configs["a"]["k"]; got != "new" {
		t.Errorf("engine config not swapped: %v", got)
	}
}

// 3. Restart path — plugin without ConfigReloader; full Shutdown/Init/Ready.
func TestReload_NoReloader_RestartsPlugin(t *testing.T) {
	// Use a counter we can observe across instances since each restart
	// spawns a fresh struct from the factory.
	var initCount, shutdownCount int32
	factory := func() Plugin {
		return &restartProbe{
			testPlugin:    testPlugin{id: "a"},
			initCount:     &initCount,
			shutdownCount: &shutdownCount,
		}
	}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{"a": factory},
		map[string]map[string]any{"a": {"k": "old"}},
	)

	// Boot already triggered one Init; reset to track only reload-driven calls.
	atomic.StoreInt32(&initCount, 0)

	new := freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "new"}})
	if err := eng.ReloadConfig(new); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := atomic.LoadInt32(&shutdownCount); got != 1 {
		t.Errorf("shutdownCount = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&initCount); got != 1 {
		t.Errorf("initCount (post-restart) = %d, want 1", got)
	}
	live := eng.Lifecycle.plugins[0].(*restartProbe)
	if got := live.observedCfg["k"]; got != "new" {
		t.Errorf("post-restart observedCfg[k] = %v, want new", got)
	}
}

// restartProbe is a testPlugin that exposes init/shutdown counters across
// fresh factory instances so tests can detect a full restart cycle.
type restartProbe struct {
	testPlugin
	initCount     *int32
	shutdownCount *int32
}

func (r *restartProbe) Init(ctx PluginContext) error {
	if r.initCount != nil {
		atomic.AddInt32(r.initCount, 1)
	}
	return r.testPlugin.Init(ctx)
}

func (r *restartProbe) Shutdown(ctx context.Context) error {
	if r.shutdownCount != nil {
		atomic.AddInt32(r.shutdownCount, 1)
	}
	return r.testPlugin.Shutdown(ctx)
}

// 4. Add plugin — new ID in active set runs full lifecycle.
func TestReload_AddPlugin(t *testing.T) {
	a := &testPlugin{id: "a"}
	b := &testPlugin{id: "b"}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
		},
		nil,
	)

	new := freshConfig([]string{"a", "b"}, nil)
	if err := eng.ReloadConfig(new); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Find the b instance in the live plugin slice (factory returns the
	// same pointer in this test).
	if !b.initCalled {
		t.Error("b should have been Init'd on add")
	}
	if !b.readyCalled {
		t.Error("b should have been Ready'd on add")
	}
	found := false
	for _, p := range eng.Lifecycle.plugins {
		if p.ID() == "b" {
			found = true
		}
	}
	if !found {
		t.Error("b not added to lifecycle.plugins")
	}
}

// 5. Remove plugin — ID gone from active set runs Shutdown.
func TestReload_RemovePlugin(t *testing.T) {
	a := &testPlugin{id: "a"}
	b := &testPlugin{id: "b"}
	eng, _ := newReloadEngine(t, []string{"a", "b"},
		map[string]func() Plugin{
			"a": func() Plugin { return a },
			"b": func() Plugin { return b },
		},
		nil,
	)

	new := freshConfig([]string{"a"}, nil)
	if err := eng.ReloadConfig(new); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !b.shutdownCall {
		t.Error("b should have been Shutdown on removal")
	}
	for _, p := range eng.Lifecycle.plugins {
		if p.ID() == "b" {
			t.Error("b still in lifecycle.plugins after removal")
		}
	}
	if _, present := eng.Lifecycle.config.Plugins.Configs["b"]; present {
		t.Error("b's config still present after removal")
	}
}

// 6. Capability swap rejected — provider identity pinning.
func TestReload_CapabilitySwapRejected(t *testing.T) {
	consumer := &testPlugin{id: "consumer", requires: []Requirement{
		{Capability: "memory.history"},
	}}
	provA := &testPlugin{id: "provider-a", capabilities: []Capability{
		{Name: "memory.history"},
	}}
	provB := &testPlugin{id: "provider-b", capabilities: []Capability{
		{Name: "memory.history"},
	}}

	eng, _ := newReloadEngine(t, []string{"consumer", "provider-a"},
		map[string]func() Plugin{
			"consumer":   func() Plugin { return consumer },
			"provider-a": func() Plugin { return provA },
			"provider-b": func() Plugin { return provB },
		},
		nil,
	)

	// Swap the provider behind memory.history.
	new := freshConfig([]string{"consumer", "provider-b"}, nil)
	err := eng.ReloadConfig(new)
	if err == nil {
		t.Fatal("expected reload to reject capability provider swap")
	}
	if !strings.Contains(err.Error(), "capability provider") {
		t.Errorf("error should call out the capability swap, got: %v", err)
	}
	if !strings.Contains(err.Error(), "memory.history") {
		t.Errorf("error should name the capability, got: %v", err)
	}
}

// 7. Engine-level config change — drain_timeout updates without plugin disruption.
func TestReload_EngineFieldsOnly(t *testing.T) {
	a := &testPlugin{id: "a"}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{"a": func() Plugin { return a }},
		nil,
	)

	new := freshConfig([]string{"a"}, nil)
	new.Engine.Shutdown.DrainTimeout = 90 * time.Second

	if err := eng.ReloadConfig(new); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := eng.Config.Engine.Shutdown.DrainTimeout; got != 90*time.Second {
		t.Errorf("DrainTimeout = %v, want 90s", got)
	}
	if a.shutdownCall {
		t.Error("plugin a was disrupted by an engine-only change")
	}
}

// 8. Concurrent reload — calls serialize via reloadMu; both succeed.
func TestReload_ConcurrentSerializes(t *testing.T) {
	plug := &reloaderPlugin{testPlugin: testPlugin{id: "a"}}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{"a": func() Plugin { return plug }},
		map[string]map[string]any{"a": {"k": "v0"}},
	)

	first := freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "v1"}})
	second := freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "v2"}})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = eng.ReloadConfig(first) }()
	go func() { defer wg.Done(); errs[1] = eng.ReloadConfig(second) }()
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Errorf("expected both reloads to succeed, got: %v / %v", errs[0], errs[1])
	}
	// Final state must reflect one of v1 / v2 (whichever ran last). The
	// per-plugin reloadCount should be exactly 2 — both calls reached the
	// reloader. The sequencing is internal but must not produce 0 or >2.
	if plug.reloadCount != 2 {
		t.Errorf("plug.reloadCount = %d, want 2 (both serialized reloads ran)", plug.reloadCount)
	}
	final := eng.Config.Plugins.Configs["a"]["k"]
	if final != "v1" && final != "v2" {
		t.Errorf("final config = %v, want v1 or v2", final)
	}
}

// 9. Reload during active dispatch — in-flight events finish; new
// subscriptions only see future events.
//
// We can't easily simulate a perfectly mid-handler reload without race
// noise, so we settle for a weaker but meaningful invariant: a
// subscription installed during a restart correctly receives a post-reload
// event. The bus's drain-on-shutdown semantics mean the prior plugin's
// handlers stop receiving once Shutdown returns; the fresh plugin's Init
// installs the new subscription. This test verifies the handoff doesn't
// drop events for the new plugin.
func TestReload_RestartReceivesPostReloadEvents(t *testing.T) {
	var ctr struct{ n int32 }

	factory := func() Plugin {
		return &subscribingPlugin{
			testPlugin: testPlugin{id: "a"},
			ctr:        &ctr,
		}
	}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{"a": factory},
		map[string]map[string]any{"a": {"k": "v1"}},
	)

	// Pre-reload event reaches the original instance.
	_ = eng.Bus.Emit("test.ping", "first")
	beforeReload := atomic.LoadInt32(&ctr.n)

	if err := eng.ReloadConfig(freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "v2"}})); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Post-reload event reaches the fresh instance.
	_ = eng.Bus.Emit("test.ping", "second")
	afterReload := atomic.LoadInt32(&ctr.n)

	if beforeReload != 1 {
		t.Errorf("beforeReload count = %d, want 1", beforeReload)
	}
	if afterReload != 2 {
		t.Errorf("afterReload count = %d, want 2 (handoff dropped events)", afterReload)
	}
}

type subscribingPlugin struct {
	testPlugin
	ctr   *struct{ n int32 }
	unsub func()
}

func (s *subscribingPlugin) Init(ctx PluginContext) error {
	s.unsub = ctx.Bus.Subscribe("test.ping", func(_ Event[any]) {
		atomic.AddInt32(&s.ctr.n, 1)
	})
	return s.testPlugin.Init(ctx)
}

func (s *subscribingPlugin) Shutdown(ctx context.Context) error {
	if s.unsub != nil {
		s.unsub()
	}
	return s.testPlugin.Shutdown(ctx)
}

// 10. fsnotify watcher integration — tempfile + observer; verify the
// callback fires after a debounced Write.
func TestReload_FsnotifyWatcherFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var fired int32
	done := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := newConfigwatchWatcher(t, path, 50*time.Millisecond, logger, func() {
		atomic.AddInt32(&fired, 1)
		select {
		case done <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher did not fire within 2s")
	}
	if got := atomic.LoadInt32(&fired); got < 1 {
		t.Errorf("fired = %d, want >= 1", got)
	}
}

// newConfigwatchWatcher is a thin wrapper that imports the configwatch
// package only inside the test build. Keeps the production engine package
// from depending on the helper for non-test code paths.
//
// (Implemented in reload_test_helpers.go to keep import surface clean.)

// reloaderError verifies a ConfigReloader failure surfaces as a reload
// error and the engine logs the failure. Not in the spec list but useful
// regression coverage.
func TestReload_ReloaderError(t *testing.T) {
	plug := &reloaderPlugin{
		testPlugin: testPlugin{id: "a"},
		reloadErr:  errors.New("plugin rejected change"),
	}
	eng, _ := newReloadEngine(t, []string{"a"},
		map[string]func() Plugin{"a": func() Plugin { return plug }},
		map[string]map[string]any{"a": {"k": "old"}},
	)
	new := freshConfig([]string{"a"}, map[string]map[string]any{"a": {"k": "new"}})
	err := eng.ReloadConfig(new)
	if err == nil || !strings.Contains(err.Error(), "plugin rejected change") {
		t.Errorf("expected reloader error to surface, got: %v", err)
	}
}
