package fallback

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func makeTestRegistry() *engine.ModelRegistry {
	return engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{
				"provider":   "nexus.llm.anthropic",
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
			},
			map[string]any{
				"provider":   "nexus.llm.openai",
				"model":      "gpt-4o",
				"max_tokens": 8192,
			},
		},
		"quick": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-haiku-4-5-20251001",
		},
	})
}

func TestFallback_VetoesProviderError(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	// Track outbound events.
	var gotClear, gotFallback, gotRetry bool
	bus.Subscribe("io.output.clear", func(_ engine.Event[any]) { gotClear = true })
	bus.Subscribe("provider.fallback", func(e engine.Event[any]) {
		fb, ok := e.Payload.(events.ProviderFallback)
		if !ok {
			t.Fatal("expected ProviderFallback payload")
		}
		if fb.NextProvider != "nexus.llm.openai" || fb.NextModel != "gpt-4o" {
			t.Fatalf("unexpected fallback target: %s/%s", fb.NextProvider, fb.NextModel)
		}
		gotFallback = true
	})
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			t.Fatal("expected LLMRequest payload")
		}
		if req.Model != "gpt-4o" {
			t.Fatalf("expected retry with gpt-4o, got %s", req.Model)
		}
		gotRetry = true
	})

	// Simulate: agent emits llm.request with before: gate.
	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced",
		Messages: []events.Message{
			{Role: "user", Content: "hello"},
		},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	// Simulate: provider fails with non-retryable error.
	errInfo := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.anthropic",
		Err:         fmt.Errorf("anthropic: API returned status 401: unauthorized"),
		Retryable:   false,
		RequestMeta: origReq.Metadata,
	}
	result, err := bus.EmitVetoable("before:core.error", errInfo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected error to be vetoed by fallback plugin")
	}

	if !gotClear {
		t.Error("expected io.output.clear to be emitted")
	}
	if !gotFallback {
		t.Error("expected provider.fallback to be emitted")
	}
	if !gotRetry {
		t.Error("expected llm.request re-emission")
	}
}

func TestFallback_ChainExhausted_NoVeto(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	// Inject request.
	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced",
		Messages: []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	// First failure → fallback to index 1.
	errInfo1 := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.anthropic",
		Err:         fmt.Errorf("anthropic: error"),
		Retryable:   false,
		RequestMeta: origReq.Metadata,
	}
	result1, _ := bus.EmitVetoable("before:core.error", errInfo1)
	if !result1.Vetoed {
		t.Fatal("expected first error to be vetoed")
	}

	// Second failure (now at index 1) → chain exhausted.
	errInfo2 := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.openai",
		Err:       fmt.Errorf("openai: error"),
		Retryable: false,
		RequestMeta: map[string]any{
			"_fallback_id":      origReq.Metadata["_fallback_id"],
			"_fallback_attempt": 1,
			"_fallback_role":    "balanced",
		},
	}
	result2, _ := bus.EmitVetoable("before:core.error", errInfo2)
	if result2.Vetoed {
		t.Fatal("expected chain-exhausted error NOT to be vetoed")
	}
}

func TestFallback_IgnoresNonProviderErrors(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	// Error from a non-provider source.
	errInfo := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.tool.shell",
		Err: fmt.Errorf("shell: command failed"),
	}
	result, _ := bus.EmitVetoable("before:core.error", errInfo)
	if result.Vetoed {
		t.Fatal("expected non-provider error to pass through")
	}
}

func TestFallback_IgnoresRetryableInProgress(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced",
		Messages: []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	// Error is retryable and retries NOT exhausted — provider still handling.
	errInfo := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.anthropic",
		Err:              fmt.Errorf("anthropic: 429 rate limited"),
		Retryable:        true,
		RetriesExhausted: false,
		RequestMeta:      origReq.Metadata,
	}
	result, _ := bus.EmitVetoable("before:core.error", errInfo)
	if result.Vetoed {
		t.Fatal("expected retryable-in-progress error to pass through")
	}
}

func TestFallback_RetriesExhausted_TriggersFallback(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced",
		Messages: []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	// Error is retryable but retries exhausted.
	errInfo := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.anthropic",
		Err:              fmt.Errorf("anthropic: max retries exceeded"),
		Retryable:        true,
		RetriesExhausted: true,
		RequestMeta:      origReq.Metadata,
	}
	result, _ := bus.EmitVetoable("before:core.error", errInfo)
	if !result.Vetoed {
		t.Fatal("expected retries-exhausted error to trigger fallback")
	}
}

func TestFallback_EmptyRole_UsesDefault(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry() // default = "balanced" which has a 2-entry chain

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	var gotRetry bool
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		if req.Model == "gpt-4o" {
			gotRetry = true
		}
	})

	// Empty role — should resolve to default ("balanced") which has fallback.
	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "",
		Messages: []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	if origReq.Metadata == nil {
		t.Fatal("expected fallback tracking metadata on empty-role request")
	}
	if _, ok := origReq.Metadata["_fallback_id"]; !ok {
		t.Fatal("expected _fallback_id in metadata")
	}

	// Simulate provider failure.
	errInfo := &events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: "nexus.llm.anthropic",
		Err:         fmt.Errorf("anthropic: error"),
		Retryable:   false,
		RequestMeta: origReq.Metadata,
	}
	result, _ := bus.EmitVetoable("before:core.error", errInfo)
	if !result.Vetoed {
		t.Fatal("expected error to be vetoed for empty-role fallback")
	}
	if !gotRetry {
		t.Fatal("expected fallback re-emission with gpt-4o")
	}
}

func TestFallback_NoChain_NoIntercept(t *testing.T) {
	bus := engine.NewEventBus()
	models := makeTestRegistry()

	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	// Request for "quick" role — only 1 entry, no fallback chain.
	origReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "quick",
		Messages: []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	// Should not have tracking metadata since chain <= 1.
	if origReq.Metadata != nil {
		if _, ok := origReq.Metadata["_fallback_id"]; ok {
			t.Fatal("expected no fallback tracking for single-entry role")
		}
	}
}
