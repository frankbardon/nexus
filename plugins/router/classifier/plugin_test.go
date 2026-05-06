package classifier

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

var testLogger = slog.Default()

func newClassifier(t *testing.T, cfg map[string]any) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: testLogger,
		Config: cfg,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, bus
}

func minimalCfg() map[string]any {
	return map[string]any{
		"classifier_model":  "tiny-judge",
		"candidates":        []any{"haiku", "sonnet", "opus"},
		"fallback":          "sonnet",
		"latency_budget_ms": 200,
	}
}

func TestInit_RequiresCandidates(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{Bus: bus, Logger: testLogger, Config: map[string]any{
		"classifier_model": "x",
	}})
	if err == nil {
		t.Fatal("expected error on missing candidates")
	}
}

func TestInit_RequiresClassifierModel(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{Bus: bus, Logger: testLogger, Config: map[string]any{
		"candidates": []any{"a"},
	}})
	if err == nil {
		t.Fatal("expected error on missing classifier_model")
	}
}

func TestSkipsInternalSourcedRequests(t *testing.T) {
	_, bus := newClassifier(t, minimalCfg())

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-sonnet-4-6-20250514",
		Messages: []events.Message{
			{Role: "user", Content: "hello"},
		},
		Metadata: map[string]any{"_source": "react"},
	}
	_, _ = bus.EmitVetoable("before:llm.request", req)

	// _source set means classifier ignored it.
	if req.Metadata["_routed_by"] != nil {
		t.Fatalf("internal-sourced requests must not be routed, got _routed_by=%v", req.Metadata["_routed_by"])
	}
	if req.Model != "claude-sonnet-4-6-20250514" {
		t.Fatalf("model unchanged expected, got %s", req.Model)
	}
}

func TestSkipsAlreadyRouted(t *testing.T) {
	_, bus := newClassifier(t, minimalCfg())

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-sonnet-4-6-20250514",
		Messages: []events.Message{
			{Role: "user", Content: "complex question"},
		},
		Metadata: map[string]any{"_routed_by": "nexus.router.metadata"},
	}
	_, _ = bus.EmitVetoable("before:llm.request", req)
	if req.Metadata["_routed_by"] != "nexus.router.metadata" {
		t.Fatalf("classifier must not overwrite metadata router decision")
	}
}

func TestCacheMissAppliesFallback(t *testing.T) {
	_, bus := newClassifier(t, minimalCfg())

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-opus-4-6-20250602",
		Messages: []events.Message{
			{Role: "user", Content: "complex question"},
		},
	}
	_, _ = bus.EmitVetoable("before:llm.request", req)
	if req.Model != "sonnet" {
		t.Fatalf("expected fallback model, got %s", req.Model)
	}
	if req.Metadata["_routed_reason"] != "fallback" {
		t.Fatalf("expected reason=fallback, got %v", req.Metadata["_routed_reason"])
	}
}

func TestCacheHitRoutesImmediately(t *testing.T) {
	p, bus := newClassifier(t, minimalCfg())
	prompt := "what is the capital of france"
	p.cache.put(promptHash(prompt, defaultPrefixChars), "haiku")

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-opus-4-6-20250602",
		Messages: []events.Message{{Role: "user", Content: prompt}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", req)
	if req.Model != "haiku" {
		t.Fatalf("expected cache hit to route to haiku, got %s", req.Model)
	}
	if req.Metadata["_routed_reason"] != "cache" {
		t.Fatalf("expected reason=cache, got %v", req.Metadata["_routed_reason"])
	}
}

func TestClassifierWarmsCacheAsync(t *testing.T) {
	p, bus := newClassifier(t, minimalCfg())

	// Stand in as the LLM provider: any llm.request from the classifier
	// gets a synthetic response with the chosen tier.
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	bus.Subscribe("llm.request", func(event engine.Event[any]) {
		req, ok := event.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		if src, _ := req.Metadata["_source"].(string); src != pluginID {
			return
		}
		callID, _ := req.Metadata["_call_id"].(string)
		go func() {
			defer probeWG.Done()
			_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "haiku",
				Metadata: map[string]any{"_call_id": callID},
			})
		}()
	})

	prompt := "trivial weather query"
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-opus-4-6-20250602",
		Messages: []events.Message{{Role: "user", Content: prompt}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", req)

	probeWG.Wait()

	// Allow goroutine to land the cache write.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := p.cache.get(promptHash(prompt, defaultPrefixChars)); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cache not warmed within deadline; cache size=%d", p.cache.len())
}

func TestResolveChoiceExactMatch(t *testing.T) {
	got := resolveChoice("haiku", []string{"haiku", "sonnet", "opus"})
	if got != "haiku" {
		t.Fatalf("got %s", got)
	}
}

func TestResolveChoiceSubstring(t *testing.T) {
	got := resolveChoice("I'd choose: opus", []string{"haiku", "sonnet", "opus"})
	if got != "opus" {
		t.Fatalf("got %s", got)
	}
}

func TestResolveChoiceUnknown(t *testing.T) {
	got := resolveChoice("none", []string{"a", "b"})
	if got != "" {
		t.Fatalf("got %s", got)
	}
}

func TestPromptHashStableWithinPrefix(t *testing.T) {
	a := promptHash("hello world abcdefg", 10)
	b := promptHash("hello worldXYZ_other_suffix", 10)
	if a != b {
		t.Fatalf("expected same hash for shared 10-char prefix, got %s vs %s", a, b)
	}
}

func TestPromptHashDistinctWhenPrefixDiffers(t *testing.T) {
	a := promptHash("aaaaaaaaaa", 10)
	b := promptHash("bbbbbbbbbb", 10)
	if a == b {
		t.Fatal("expected distinct hashes")
	}
}

func TestCacheLRU(t *testing.T) {
	c := newCache(2)
	c.put("a", "1")
	c.put("b", "2")
	c.put("c", "3") // evicts a
	if _, ok := c.get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Fatal("b should still be present")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("c should still be present")
	}
}
