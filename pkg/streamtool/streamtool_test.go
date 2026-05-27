package streamtool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

type fakeTool struct {
	name   string
	emit   []ToolEvent
	delay  time.Duration
	stream func() error
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Stream(ctx context.Context, _ map[string]any) (<-chan ToolEvent, error) {
	ch := make(chan ToolEvent, len(f.emit)+1)
	go func() {
		defer close(ch)
		for _, ev := range f.emit {
			if f.delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(f.delay):
				}
			}
			ch <- ev
			if ev.Kind == KindComplete || ev.Kind == KindError {
				return
			}
		}
	}()
	return ch, nil
}

func TestBridge_PartialsThenComplete(t *testing.T) {
	bus := engine.NewEventBus()
	tool := &fakeTool{
		name: "long_compute",
		emit: []ToolEvent{
			{Kind: KindProgress, Sequence: 1, Progress: 0.1},
			{Kind: KindPartial, Sequence: 2, Payload: "step1"},
			{Kind: KindPartial, Sequence: 3, Payload: "step2"},
			{Kind: KindComplete, Sequence: 4, Payload: "final answer"},
		},
	}

	var (
		mu        sync.Mutex
		progress  int
		partials  int
		gotResult events.ToolResult
		done      = make(chan struct{}, 1)
	)
	bus.Subscribe("tool.stream.progress", func(_ engine.Event[any]) {
		mu.Lock()
		progress++
		mu.Unlock()
	})
	bus.Subscribe("tool.stream.partial", func(_ engine.Event[any]) {
		mu.Lock()
		partials++
		mu.Unlock()
	})
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			mu.Lock()
			gotResult = r
			mu.Unlock()
			done <- struct{}{}
		}
	})

	call := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          tool.name,
		Arguments:     map[string]any{},
		TurnID:        "turn-1",
	}
	if err := Bridge(context.Background(), bus, tool, call); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("no tool.result")
	}

	mu.Lock()
	defer mu.Unlock()
	if progress != 1 {
		t.Errorf("progress = %d, want 1", progress)
	}
	if partials != 2 {
		t.Errorf("partials = %d, want 2", partials)
	}
	if gotResult.Output != "final answer" {
		t.Errorf("Output = %q", gotResult.Output)
	}
}

func TestBridge_ErrorPath(t *testing.T) {
	bus := engine.NewEventBus()
	tool := &fakeTool{
		name: "fails",
		emit: []ToolEvent{{Kind: KindError, Err: errors.New("boom")}},
	}
	var gotResult events.ToolResult
	done := make(chan struct{}, 1)
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			gotResult = r
			done <- struct{}{}
		}
	})

	call := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c", Name: tool.name, TurnID: "t"}
	if err := Bridge(context.Background(), bus, tool, call); err == nil {
		t.Errorf("expected error from bridge")
	}
	<-done
	if gotResult.Error != "boom" {
		t.Errorf("Error = %q", gotResult.Error)
	}
}

func TestBridge_ContextCancel(t *testing.T) {
	bus := engine.NewEventBus()
	tool := &fakeTool{
		name:  "slow",
		emit:  []ToolEvent{{Kind: KindPartial, Sequence: 1}, {Kind: KindComplete, Sequence: 2}},
		delay: 200 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	call := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c", Name: tool.name, TurnID: "t"}
	err := Bridge(ctx, bus, tool, call)
	if err == nil {
		t.Errorf("expected context error")
	}
}
