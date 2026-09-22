package fallback

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// makeOverridesRegistry builds a two-entry chain whose entries carry different
// native provider blocks, so a test can tell the fallback entry's block apart
// from the primary's — the thing a registry lookup at the provider could not.
func makeOverridesRegistry() *engine.ModelRegistry {
	return engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{
				"provider": "nexus.llm.anthropic",
				"model":    "claude-sonnet-4-20250514",
				"api":      "messages",
				"thinking": map[string]any{"mode": "adaptive"},
				"cache":    map[string]any{"enabled": true},
			},
			map[string]any{
				"provider":    "nexus.llm.gemini",
				"model":       "gemini-2.5-pro",
				"api":         "generate",
				"temperature": 0,
				"thinking": map[string]any{
					"mode":          "budget",
					"budget_tokens": 2048,
				},
			},
		},
	})
}

func TestFallback_ChainEntryBlocksReachRetryRequest(t *testing.T) {
	retry := runFallbackOnce(t, makeOverridesRegistry(), "")

	if retry.Model != "gemini-2.5-pro" {
		t.Fatalf("retry model = %q, want the fallback entry's model", retry.Model)
	}
	if !retry.Overrides.Stamped {
		t.Fatal("retry request must be marked stamped so the provider skips the registry")
	}
	if got := retry.Overrides.API; got != "generate" {
		t.Fatalf("retry api = %q, want the fallback entry's %q (not the primary's)", got, "generate")
	}
	if got := retry.Overrides.Thinking["mode"]; got != "budget" {
		t.Fatalf("retry thinking.mode = %v, want the fallback entry's %q (not the primary's %q)",
			got, "budget", "adaptive")
	}
	if got := retry.Overrides.Thinking["budget_tokens"]; got != 2048 {
		t.Fatalf("retry thinking.budget_tokens = %v, want 2048", got)
	}
	if retry.Temperature == nil || *retry.Temperature != 0 {
		t.Fatalf("retry temperature = %v, want the fallback entry's explicit 0", retry.Temperature)
	}
	// The primary's cache block must not leak onto an entry that sets none.
	if retry.Overrides.Cache != nil {
		t.Fatalf("retry cache = %v, want nil — only the primary sets one", retry.Overrides.Cache)
	}
}

// runFallbackWith drives one primary failure through the coordinator with a
// caller-shaped inbound request, and returns the retry request it re-emitted.
func runFallbackWith(t *testing.T, models *engine.ModelRegistry, shape func(*events.LLMRequest)) events.LLMRequest {
	t.Helper()

	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.models = models
	p.logger = slog.Default()

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	var retry events.LLMRequest
	var gotRetry bool
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			t.Errorf("expected LLMRequest payload, got %T", e.Payload)
			return
		}
		retry = req
		gotRetry = true
	})

	origReq := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "balanced",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}
	shape(origReq)
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	errInfo := &events.ErrorInfo{
		SchemaVersion: events.ErrorInfoVersion,
		Source:        "nexus.llm.anthropic",
		Err:           fmt.Errorf("anthropic: API returned status 401: unauthorized"),
		Retryable:     false,
		RequestMeta:   origReq.Metadata,
	}
	if _, err := bus.EmitVetoable("before:core.error", errInfo); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotRetry {
		t.Fatal("expected a retry llm.request to be re-emitted")
	}
	return retry
}

func TestFallback_InboundOverridesSurviveUntouched(t *testing.T) {
	retry := runFallbackWith(t, makeOverridesRegistry(), func(req *events.LLMRequest) {
		req.Overrides.API = "caller-api"
		req.Overrides.Thinking = map[string]any{"mode": "caller"}
	})

	if got := retry.Overrides.API; got != "caller-api" {
		t.Fatalf("retry api = %q, want the inbound value to survive the chain entry", got)
	}
	if got := retry.Overrides.Thinking["mode"]; got != "caller" {
		t.Fatalf("retry thinking.mode = %v, want the inbound block to survive the chain entry", got)
	}
	// An axis the caller left alone is still filled by the entry.
	if retry.Temperature == nil || *retry.Temperature != 0 {
		t.Fatalf("retry temperature = %v, want the entry to fill the gap", retry.Temperature)
	}
}

func TestFallback_EntryWithoutBlocksLeavesRequestUnset(t *testing.T) {
	retry := runFallbackOnce(t, makeTestRegistry(), "")

	if retry.Overrides.Thinking != nil || retry.Overrides.Reasoning != nil ||
		retry.Overrides.Cache != nil || retry.Overrides.Retry != nil ||
		retry.Overrides.API != "" {
		t.Fatalf("retry overrides = %+v, want every axis unset when the entry sets none",
			retry.Overrides)
	}
	if !retry.Overrides.Stamped {
		t.Fatal("a stamp with nothing to fill is still a stamp")
	}
}
