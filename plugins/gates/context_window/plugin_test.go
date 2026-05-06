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
