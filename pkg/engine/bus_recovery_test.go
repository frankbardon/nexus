package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// withForcedOrigin temporarily replaces the panic-origin classifier so tests
// in the engine package — whose handlers live outside /plugins/ — can drive
// the recovery wrapper end-to-end. Restored automatically via t.Cleanup.
func withForcedOrigin(t *testing.T, isPlugin bool) {
	t.Helper()
	prev := originClassifier
	originClassifier = func([]byte) bool { return isPlugin }
	t.Cleanup(func() { originClassifier = prev })
}

// newBusWithLogger builds a bus with a buffered slog handler attached so
// tests can assert on logged fields. Returns the bus, the buffer, and the
// logger (kept around so callers can re-use it for additional plugins).
func newBusWithLogger() (*eventBus, *bytes.Buffer) {
	bus := NewEventBus().(*eventBus)
	buf := &bytes.Buffer{}
	bus.SetLogger(slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug})))
	return bus, buf
}

// TestPanicRecovery_RegularHandler_EmitsCoreErrorAndContinues covers the
// foundational case: a panic in one handler is recovered, surfaced as a
// core.error event, and does not block the other subscribers from receiving
// the original event.
func TestPanicRecovery_RegularHandler_EmitsCoreErrorAndContinues(t *testing.T) {
	withForcedOrigin(t, true)
	bus, buf := newBusWithLogger()

	var coreErr atomic.Value
	bus.Subscribe("core.error", func(e Event[any]) {
		if info, ok := e.Payload.(events.ErrorInfo); ok {
			coreErr.Store(info)
		}
	})

	var laterRan atomic.Bool
	bus.Subscribe("test.event", func(e Event[any]) {
		panic("boom")
	}, WithPriority(10), WithSource("nexus.plugin.bad"))
	bus.Subscribe("test.event", func(e Event[any]) {
		laterRan.Store(true)
	}, WithPriority(20))

	if err := bus.Emit("test.event", "hi"); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}

	if !laterRan.Load() {
		t.Fatal("subsequent handler should still run after a panic was recovered")
	}

	v := coreErr.Load()
	if v == nil {
		t.Fatal("expected core.error event after recovered panic")
	}
	info := v.(events.ErrorInfo)
	if info.Source != "nexus.plugin.bad" {
		t.Errorf("ErrorInfo.Source = %q, want nexus.plugin.bad", info.Source)
	}
	if info.EventType != "test.event" {
		t.Errorf("ErrorInfo.EventType = %q, want test.event", info.EventType)
	}
	if info.Stack == "" {
		t.Error("ErrorInfo.Stack should be populated with debug.Stack output")
	}
	if !strings.Contains(info.Err.Error(), "boom") {
		t.Errorf("ErrorInfo.Err = %v, want it to mention 'boom'", info.Err)
	}
	if logs := buf.String(); !strings.Contains(logs, "plugin handler panic recovered") {
		t.Errorf("expected structured panic-recovered log, got: %s", logs)
	}
}

// TestPanicRecovery_VetoableHandler_FailsClosed verifies that a panic in a
// before:* handler is treated as a veto with reason "plugin panic: ..." so
// the gated action is blocked rather than allowed through silently.
func TestPanicRecovery_VetoableHandler_FailsClosed(t *testing.T) {
	withForcedOrigin(t, true)
	bus, _ := newBusWithLogger()

	bus.Subscribe("before:test.action", func(e Event[any]) {
		panic("guard exploded")
	}, WithSource("nexus.plugin.gate"))

	payload := "subject"
	result, err := bus.EmitVetoable("before:test.action", &payload)
	if err != nil {
		t.Fatalf("EmitVetoable error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected veto when before:* handler panicked, got pass")
	}
	if !strings.Contains(result.Reason, "plugin panic") {
		t.Errorf("veto reason = %q, want it to mention 'plugin panic'", result.Reason)
	}
	if !strings.Contains(result.Reason, "guard exploded") {
		t.Errorf("veto reason = %q, want it to include the recovered panic value", result.Reason)
	}
}

// TestPanicRecovery_RecursionGuard ensures a handler subscribed to core.error
// that itself panics does not trigger another core.error emit. Without the
// dispatchInError guard the second panic would loop indefinitely.
func TestPanicRecovery_RecursionGuard(t *testing.T) {
	withForcedOrigin(t, true)
	bus, buf := newBusWithLogger()

	var coreErrorCount atomic.Int32
	bus.Subscribe("core.error", func(e Event[any]) {
		coreErrorCount.Add(1)
		panic("observer panic")
	}, WithSource("nexus.plugin.observer"))

	bus.Subscribe("test.event", func(e Event[any]) {
		panic("primary panic")
	}, WithSource("nexus.plugin.bad"))

	done := make(chan struct{})
	go func() {
		_ = bus.Emit("test.event", nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recursion guard failed: emit did not return — likely infinite loop")
	}

	if got := coreErrorCount.Load(); got != 1 {
		t.Errorf("core.error handler should run exactly once, ran %d times", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "panic during core.error dispatch") {
		t.Errorf("expected recursion-guard log, got: %s", logs)
	}
}

// TestPanicRecovery_EngineOriginRePanics verifies the stack-frame origin rule:
// when the panic originates outside of plugin code (the originClassifier
// returns false) the wrapper re-raises so engine bugs surface loudly rather
// than being masked.
func TestPanicRecovery_EngineOriginRePanics(t *testing.T) {
	withForcedOrigin(t, false)
	bus, _ := newBusWithLogger()

	bus.Subscribe("test.event", func(e Event[any]) {
		panic("engine bug")
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected re-panic for engine-origin handler, got none")
		}
		if !strings.Contains(toString(r), "engine bug") {
			t.Errorf("re-panic value = %v, want 'engine bug'", r)
		}
	}()
	_ = bus.Emit("test.event", nil)
}

// TestPanicRecovery_FailFastBypass verifies SetFailFast disables the wrapper
// entirely so panics propagate with their original stack trace.
func TestPanicRecovery_FailFastBypass(t *testing.T) {
	withForcedOrigin(t, true) // would normally recover, but failFast wins
	bus, _ := newBusWithLogger()
	bus.SetFailFast(true)

	bus.Subscribe("test.event", func(e Event[any]) {
		panic("flaky")
	}, WithSource("nexus.plugin.flaky"))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate when failFast is on, got none")
		}
		if !strings.Contains(toString(r), "flaky") {
			t.Errorf("re-panic value = %v, want 'flaky'", r)
		}
	}()
	_ = bus.Emit("test.event", nil)
}

// TestPanicFromPlugin_Classifier exercises the production stack classifier
// against synthesized debug.Stack-style frames so the heuristic regresses
// loudly if someone reorders the path checks.
func TestPanicFromPlugin_Classifier(t *testing.T) {
	cases := []struct {
		name  string
		stack string
		want  bool
	}{
		{
			name: "plugin frame",
			stack: "goroutine 1 [running]:\n" +
				"main.handler(...)\n" +
				"\t/Users/u/code/nexus/plugins/foo/bar.go:42 +0x10\n" +
				"main.main()\n" +
				"\t/Users/u/code/main.go:8 +0x80\n",
			want: true,
		},
		{
			name: "engine frame only",
			stack: "goroutine 1 [running]:\n" +
				"main.handler(...)\n" +
				"\t/Users/u/code/nexus/pkg/engine/lifecycle.go:300 +0x10\n",
			want: false,
		},
		{
			name: "runtime frames skipped, plugin wins",
			stack: "goroutine 1 [running]:\n" +
				"runtime.gopanic(...)\n" +
				"\t/usr/local/go/src/runtime/panic.go:884 +0x213\n" +
				"main.handler(...)\n" +
				"\t/Users/u/code/nexus/plugins/agents/react/loop.go:99 +0x10\n",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := panicFromPlugin([]byte(tc.stack)); got != tc.want {
				t.Errorf("panicFromPlugin = %v, want %v", got, tc.want)
			}
		})
	}
}

// drainPlugin fakes a slow shutdown via the DrainOverride interface. Implements
// the minimum Plugin surface so it can be appended to lm.plugins for the
// drain-resolution test.
type drainPlugin struct {
	id      string
	timeout time.Duration
}

func (d *drainPlugin) ID() string                         { return d.id }
func (d *drainPlugin) Name() string                       { return d.id }
func (d *drainPlugin) Version() string                    { return "test" }
func (d *drainPlugin) Dependencies() []string             { return nil }
func (d *drainPlugin) Requires() []Requirement            { return nil }
func (d *drainPlugin) Capabilities() []Capability         { return nil }
func (d *drainPlugin) Init(_ PluginContext) error         { return nil }
func (d *drainPlugin) Ready() error                       { return nil }
func (d *drainPlugin) Shutdown(_ context.Context) error   { return nil }
func (d *drainPlugin) Subscriptions() []EventSubscription { return nil }
func (d *drainPlugin) Emissions() []string                { return nil }
func (d *drainPlugin) DrainTimeout() time.Duration        { return d.timeout }

// TestResolveDrainTimeout_Default falls back to the engine-default ceiling
// when the config is empty and no plugin advertises a longer drain.
func TestResolveDrainTimeout_Default(t *testing.T) {
	lm := &LifecycleManager{config: &Config{}}
	if got := lm.resolveDrainTimeout(); got != defaultShutdownDrainTimeout {
		t.Errorf("default drain = %s, want %s", got, defaultShutdownDrainTimeout)
	}
}

// TestResolveDrainTimeout_ConfigOverride applies engine.shutdown.drain_timeout
// when set higher than the default.
func TestResolveDrainTimeout_ConfigOverride(t *testing.T) {
	lm := &LifecycleManager{config: &Config{
		Engine: EngineConfig{Shutdown: ShutdownConfig{DrainTimeout: 90 * time.Second}},
	}}
	if got := lm.resolveDrainTimeout(); got != 90*time.Second {
		t.Errorf("configured drain = %s, want 90s", got)
	}
}

// TestResolveDrainTimeout_PluginOverrideExtends verifies a plugin advertising
// a longer drain than the engine default extends the effective window — the
// configured engine ceiling is taken as the floor.
func TestResolveDrainTimeout_PluginOverrideExtends(t *testing.T) {
	lm := &LifecycleManager{
		config:  &Config{Engine: EngineConfig{Shutdown: ShutdownConfig{DrainTimeout: 30 * time.Second}}},
		plugins: []Plugin{&drainPlugin{id: "slow", timeout: 90 * time.Second}},
	}
	if got := lm.resolveDrainTimeout(); got != 90*time.Second {
		t.Errorf("plugin-extended drain = %s, want 90s", got)
	}
}

// TestResolveDrainTimeout_PluginOverrideShorterIgnored verifies a plugin that
// requests a shorter drain than the engine default has no effect — the engine
// default acts as a floor.
func TestResolveDrainTimeout_PluginOverrideShorterIgnored(t *testing.T) {
	lm := &LifecycleManager{
		config:  &Config{Engine: EngineConfig{Shutdown: ShutdownConfig{DrainTimeout: 30 * time.Second}}},
		plugins: []Plugin{&drainPlugin{id: "fast", timeout: 5 * time.Second}},
	}
	if got := lm.resolveDrainTimeout(); got != 30*time.Second {
		t.Errorf("plugin-shorter drain = %s, want 30s (engine floor)", got)
	}
}

// TestShutdown_DrainTimeoutExceeded forces a stuck handler so the drain
// budget elapses and Shutdown logs the timeout but still completes.
func TestShutdown_DrainTimeoutExceeded(t *testing.T) {
	cfg := &Config{
		Engine: EngineConfig{Shutdown: ShutdownConfig{DrainTimeout: 100 * time.Millisecond}},
		Plugins: PluginsConfig{
			Active:  []string{},
			Configs: map[string]map[string]any{},
		},
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := NewEventBus().(*eventBus)
	lm := NewLifecycleManager(NewPluginRegistry(), bus, cfg, logger, nil, nil, nil, nil)

	// Subscribe a wildcard handler that blocks long enough to outlast the
	// drain budget. Started in a goroutine via Emit; Shutdown should observe
	// the drain timeout.
	release := make(chan struct{})
	defer close(release)
	bus.Subscribe("test.stuck", func(e Event[any]) {
		<-release
	})

	emitDone := make(chan struct{})
	go func() {
		_ = bus.Emit("test.stuck", nil)
		close(emitDone)
	}()
	// Give the goroutine a moment to enter the handler.
	time.Sleep(10 * time.Millisecond)

	start := time.Now()
	if err := lm.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Errorf("Shutdown took %s; expected drain timeout (100ms) to bound it", elapsed)
	}
	if logs := buf.String(); !strings.Contains(logs, "drain timed out") {
		t.Errorf("expected 'drain timed out' log, got: %s", logs)
	}
}

// toString renders a recover() value as a string for assertion convenience.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}
