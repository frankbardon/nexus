package endlessloop

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

var testLogger = slog.Default()

func TestEndlessLoop_VetoesAtMax(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	p.maxIterations = 3

	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "hi"}}}

	// First 3 should pass.
	for i := 1; i <= 3; i++ {
		result, err := bus.EmitVetoable("before:llm.request", &req)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if result.Vetoed {
			t.Fatalf("iteration %d: should not veto yet", i)
		}
	}

	// 4th should veto.
	result, err := bus.EmitVetoable("before:llm.request", &req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected veto at iteration 4")
	}
}

func TestEndlessLoop_ResetsOnInput(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	p.maxIterations = 2

	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))
	bus.Subscribe("io.input", p.handleInput, engine.WithPriority(5))

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "hi"}}}

	// Use 2 iterations.
	bus.EmitVetoable("before:llm.request", &req)
	bus.EmitVetoable("before:llm.request", &req)

	// Simulate new user input — should reset.
	bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "new message"})

	// Should pass again.
	result, _ := bus.EmitVetoable("before:llm.request", &req)
	if result.Vetoed {
		t.Fatal("should not veto after reset")
	}
}

func TestEndlessLoop_SkipsSourcedRequests(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	p.maxIterations = 1

	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))

	// Sourced request (from planner) should not count.
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "hi"}},
		Metadata: map[string]any{"_source": "nexus.planner.dynamic"},
	}

	for i := 0; i < 5; i++ {
		result, _ := bus.EmitVetoable("before:llm.request", &req)
		if result.Vetoed {
			t.Fatalf("sourced request should not be counted, vetoed at iteration %d", i)
		}
	}
}

func TestEndlessLoop_Warning(t *testing.T) {
	bus := engine.NewEventBus()
	var warnings []string
	bus.Subscribe("io.output", func(e engine.Event[any]) {
		if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "system" {
			warnings = append(warnings, out.Content)
		}
	})

	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	p.maxIterations = 5
	p.warningAt = 3

	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{{Role: "user", Content: "hi"}}}

	for i := 0; i < 3; i++ {
		bus.EmitVetoable("before:llm.request", &req)
	}

	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
}
