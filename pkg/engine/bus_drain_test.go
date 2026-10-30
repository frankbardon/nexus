package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestDrain_NestedEmitDuringDrain is the #172/#202 regression: a handler
// that is in flight when Drain starts emits a nested event. The nested emit
// must not block behind Drain, and Drain must return nil once the tree
// finishes.
func TestDrain_NestedEmitDuringDrain(t *testing.T) {
	bus := NewEventBus()

	entered := make(chan struct{})
	release := make(chan struct{})
	nestedDone := make(chan struct{})
	bus.Subscribe("test.outer", func(e Event[any]) {
		close(entered)
		<-release
		_ = bus.Emit("test.nested", nil)
		close(nestedDone)
	})
	bus.Subscribe("test.nested", func(e Event[any]) {
		_, _ = bus.EmitVetoable("before:test.nested2", nil)
	})

	go func() { _ = bus.Emit("test.outer", nil) }()
	<-entered

	drainErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drainErr <- bus.Drain(ctx)
	}()
	// Let Drain start waiting before the handler emits its nested event.
	time.Sleep(10 * time.Millisecond)
	close(release)

	select {
	case <-nestedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("nested emit blocked while Drain was waiting")
	}
	select {
	case err := <-drainErr:
		if err != nil {
			t.Fatalf("Drain = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the in-flight tree completed")
	}
}

// TestDrain_TimeoutLeavesBusUsable: Drain gives up on a stuck handler at
// ctx expiry and must leave nothing behind that a later Emit waits on.
func TestDrain_TimeoutLeavesBusUsable(t *testing.T) {
	bus := NewEventBus()

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	bus.Subscribe("test.stuck", func(e Event[any]) {
		close(entered)
		<-release
	})
	go func() { _ = bus.Emit("test.stuck", nil) }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := bus.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want DeadlineExceeded", err)
	}

	got := make(chan struct{}, 1)
	bus.Subscribe("test.after", func(e Event[any]) { got <- struct{}{} })
	emitted := make(chan struct{})
	go func() {
		_ = bus.Emit("test.after", nil)
		_, _ = bus.EmitVetoable("before:test.after", nil)
		close(emitted)
	}()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit after a timed-out Drain blocked")
	}
	select {
	case <-got:
	default:
		t.Fatal("handler for post-drain emit did not run")
	}
}

// TestDrain_IdleBusReturnsImmediately covers the zero-in-flight fast path,
// including a bus that has been busy and gone quiet again.
func TestDrain_IdleBusReturnsImmediately(t *testing.T) {
	bus := NewEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // an already-done ctx must still yield nil on an idle bus
	if err := bus.Drain(ctx); err != nil {
		t.Fatalf("Drain on fresh bus = %v", err)
	}
	_ = bus.Emit("test.x", nil)
	if err := bus.Drain(ctx); err != nil {
		t.Fatalf("Drain after quiesce = %v", err)
	}
}

// shutdownEmitPlugin emits from its Shutdown, as logging/journal/io plugins
// do during teardown — the call that hung forever in #202.
type shutdownEmitPlugin struct {
	drainPlugin
	bus EventBus
}

func (p *shutdownEmitPlugin) Shutdown(_ context.Context) error {
	return p.bus.Emit("test.teardown", nil)
}

// TestShutdown_NestedEmitAndStuckHandlerBounded is the lifecycle-level
// regression for #202: at shutdown one handler is stuck and another
// in-flight handler emits nested events. Shutdown must return within the
// drain budget, and a plugin that emits from Shutdown must not hang.
func TestShutdown_NestedEmitAndStuckHandlerBounded(t *testing.T) {
	cfg := &Config{
		Engine:  EngineConfig{Shutdown: ShutdownConfig{DrainTimeout: 100 * time.Millisecond}},
		Plugins: PluginsConfig{Active: []string{}, Configs: map[string]map[string]any{}},
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(io.Writer(buf), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := NewEventBus().(*eventBus)
	lm := NewLifecycleManager(NewPluginRegistry(), bus, cfg, logger, nil, nil, nil, nil)
	lm.plugins = append(lm.plugins, &shutdownEmitPlugin{drainPlugin: drainPlugin{id: "test.emitter"}, bus: bus})

	stuck := make(chan struct{})
	defer close(stuck)
	stuckEntered := make(chan struct{})
	bus.Subscribe("test.stuck", func(e Event[any]) {
		close(stuckEntered)
		<-stuck
	})
	// A handler that keeps emitting nested events across the drain window.
	nestedEntered := make(chan struct{})
	bus.Subscribe("test.chatty", func(e Event[any]) {
		close(nestedEntered)
		for i := 0; i < 5; i++ {
			_ = bus.Emit("test.chatty.nested", i)
			time.Sleep(10 * time.Millisecond)
		}
	})
	go func() { _ = bus.Emit("test.stuck", nil) }()
	go func() { _ = bus.Emit("test.chatty", nil) }()
	<-stuckEntered
	<-nestedEntered

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- lm.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown hung past the drain budget")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Shutdown took %s; drain budget is 100ms", elapsed)
	}
	if !strings.Contains(buf.String(), "drain timed out") {
		t.Errorf("expected 'drain timed out' log, got: %s", buf.String())
	}
}
