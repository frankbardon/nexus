package contextwindow

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func TestContextWindow_UnderThreshold_NoVeto(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:              bus,
		logger:           slog.Default(),
		maxContextTokens: 100000,
		triggerRatio:     0.85,
		charsPerToken:    4.0,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(15)),
	)

	// Small message — well under threshold.
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "user", Content: "Hello"},
	},
	}

	veto, err := bus.EmitVetoable("before:llm.request", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if veto.Vetoed {
		t.Fatal("should not veto small context")
	}
}

func TestContextWindow_OverThreshold_Vetoes(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:              bus,
		logger:           slog.Default(),
		maxContextTokens: 100, // tiny limit
		triggerRatio:     0.5, // triggers at 50 tokens
		charsPerToken:    1.0, // 1 char = 1 token for easy math
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(15)),
	)

	// Suppress io.output.
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})
	bus.Subscribe("memory.compact.request", func(_ engine.Event[any]) {})

	// 60 chars > 50 token threshold.
	bigContent := make([]byte, 60)
	for i := range bigContent {
		bigContent[i] = 'a'
	}
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "user", Content: string(bigContent)},
	},
	}

	veto, err := bus.EmitVetoable("before:llm.request", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected veto when over context threshold")
	}
}

func TestContextWindow_SkipsSourcedRequests(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:              bus,
		logger:           slog.Default(),
		maxContextTokens: 10, // tiny limit
		triggerRatio:     0.5,
		charsPerToken:    1.0,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(15)),
	)

	// Sourced request (e.g. from compaction plugin) should not be vetoed.
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "user", Content: "a long message that exceeds the limit easily"},
	},
		Metadata: map[string]any{"_source": "nexus.memory.compaction"},
	}

	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if veto.Vetoed {
		t.Fatal("should not veto sourced requests")
	}
}

func TestContextWindow_EmitsRetryAfterCompaction(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:              bus,
		logger:           slog.Default(),
		maxContextTokens: 10,
		triggerRatio:     0.5,
		charsPerToken:    1.0,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(15)),
		bus.Subscribe("memory.compacted", p.handleMemoryCompacted,
			engine.WithPriority(60)),
	)

	// Suppress side-effect events.
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})
	bus.Subscribe("memory.compact.request", func(_ engine.Event[any]) {})

	var mu sync.Mutex
	retryCount := 0
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {
		mu.Lock()
		retryCount++
		mu.Unlock()
	})

	// Trigger a veto (sets pendingRetry = true).
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "user", Content: "a long message that exceeds limit"},
	},
	}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if !veto.Vetoed {
		t.Fatal("expected veto")
	}

	// Simulate compaction completing.
	bus.Emit("memory.compacted", events.CompactionComplete{SchemaVersion: events.CompactionCompleteVersion, Messages: []events.Message{{Role: "user", Content: "short"}}})

	mu.Lock()
	got := retryCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected 1 gate.llm.retry after compaction, got %d", got)
	}
}

func TestContextWindow_NoRetryWithoutPriorVeto(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:              bus,
		logger:           slog.Default(),
		maxContextTokens: 100000,
		triggerRatio:     0.85,
		charsPerToken:    4.0,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("memory.compacted", p.handleMemoryCompacted,
			engine.WithPriority(60)),
	)

	retryCount := 0
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {
		retryCount++
	})

	// Compaction fires but we never vetoed — should not retry.
	bus.Emit("memory.compacted", events.CompactionComplete{SchemaVersion: events.CompactionCompleteVersion, Messages: []events.Message{{Role: "user", Content: "hi"}}})

	if retryCount != 0 {
		t.Fatalf("expected 0 retries without prior veto, got %d", retryCount)
	}
}

// TestContextWindow_GatesConversationTurns pins that the agent loops' own
// turns are measured. They stamp _source (cost attribution, #83), and until
// E1-S15 this gate skipped any non-empty _source — so it never compacted.
func TestContextWindow_GatesConversationTurns(t *testing.T) {
	for _, meta := range []map[string]any{
		{"_source": "nexus.agent.react", "task_kind": "react_main"},
		{"_source": "nexus.agent.planexec.executor", "task_kind": "planexec_step"},
	} {
		bus := engine.NewEventBus()
		p := &Plugin{bus: bus, logger: slog.Default(), maxContextTokens: 10, triggerRatio: 0.5, charsPerToken: 1.0}
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(15))
		req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Metadata: meta,
			Messages: []events.Message{{Role: "user", Content: "a long message that exceeds the limit easily"}}}
		if veto, _ := bus.EmitVetoable("before:llm.request", req); !veto.Vetoed {
			t.Errorf("%v: over-threshold agent turn not gated", meta["task_kind"])
		}
	}
}

// TestContextWindow_SkipsNestedAgentLoops pins that subagent and delegate
// requests are not measured: their history is in-process, so compacting the
// main history would not shrink it, and the veto would fail the nested call.
func TestContextWindow_SkipsNestedAgentLoops(t *testing.T) {
	for _, meta := range []map[string]any{
		{"_source": "nexus.agent.subagent.s1", "task_kind": "subagent"},
		{"_source": "delegate.sub-1", "task_kind": "delegate"},
		{"_source": "nexus.memory.compaction", "task_kind": engine.TaskKindCompaction},
	} {
		bus := engine.NewEventBus()
		p := &Plugin{bus: bus, logger: slog.Default(), maxContextTokens: 10, triggerRatio: 0.5, charsPerToken: 1.0}
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(15))
		req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Metadata: meta,
			Messages: []events.Message{{Role: "user", Content: "a long message that exceeds the limit easily"}}}
		if veto, _ := bus.EmitVetoable("before:llm.request", req); veto.Vetoed {
			t.Errorf("%v: request should not be gated", meta["task_kind"])
		}
	}
}
