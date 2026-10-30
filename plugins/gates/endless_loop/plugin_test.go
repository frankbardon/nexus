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

// TestEndlessLoop_CountsAgentLoopTurns pins that the agent loops' own
// iterations count. They stamp _source on every turn (cost attribution,
// #83), and until E1-S15 this gate skipped any non-empty _source — so a
// react turn could iterate forever and the limit never fired. Nested loops
// (subagent, delegate) count toward the parent turn: they run inside it and
// rely on this gate as their iteration cap.
func TestEndlessLoop_CountsAgentLoopTurns(t *testing.T) {
	for _, meta := range []map[string]any{
		{"_source": "nexus.agent.react", "task_kind": "react_main"},
		{"_source": "nexus.agent.planexec.executor", "task_kind": "planexec_step"},
		{"_source": "nexus.agent.orchestrator.decompose", "task_kind": "orchestrator_decompose"},
		{"_source": "nexus.agent.subagent.s1", "task_kind": "subagent"},
		{"_source": "delegate.sub-1", "task_kind": "delegate"},
	} {
		bus := engine.NewEventBus()
		p := New().(*Plugin)
		p.bus = bus
		p.logger = testLogger
		p.maxIterations = 3
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(10))

		req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Metadata: meta}
		for i := 1; i <= 3; i++ {
			if res, _ := bus.EmitVetoable("before:llm.request", &req); res.Vetoed {
				t.Fatalf("%v: vetoed at iteration %d, limit 3", meta["task_kind"], i)
			}
		}
		if res, _ := bus.EmitVetoable("before:llm.request", &req); !res.Vetoed {
			t.Fatalf("%v: iteration 4 not vetoed — agent-loop turns are not counted", meta["task_kind"])
		}
	}
}

// TestEndlessLoop_InternalRequestsDoNotCount pins that internal requests
// interleaved with a turn's iterations do not consume its budget.
func TestEndlessLoop_InternalRequestsDoNotCount(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	p.maxIterations = 2
	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(10))

	turn := events.LLMRequest{SchemaVersion: events.LLMRequestVersion,
		Metadata: map[string]any{"_source": "nexus.agent.react", "task_kind": "react_main"}}
	internal := events.LLMRequest{SchemaVersion: events.LLMRequestVersion,
		Metadata: map[string]any{"_source": "nexus.memory.compaction", "task_kind": engine.TaskKindCompaction}}
	for i := 0; i < 2; i++ {
		if res, _ := bus.EmitVetoable("before:llm.request", &turn); res.Vetoed {
			t.Fatalf("turn %d vetoed", i)
		}
		for j := 0; j < 3; j++ {
			if res, _ := bus.EmitVetoable("before:llm.request", &internal); res.Vetoed {
				t.Fatal("internal request vetoed")
			}
		}
	}
}
