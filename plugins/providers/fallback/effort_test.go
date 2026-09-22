package fallback

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// makeEffortRegistry builds a two-entry chain whose fallback entry carries its
// own effort, distinct from the primary's.
func makeEffortRegistry() *engine.ModelRegistry {
	return engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{
				"provider": "nexus.llm.anthropic",
				"model":    "claude-sonnet-4-20250514",
				"effort":   "high",
			},
			map[string]any{
				"provider": "nexus.llm.gemini",
				"model":    "gemini-2.5-pro",
				"effort":   "minimal",
			},
		},
	})
}

// runFallbackOnce drives one primary failure through the coordinator and
// returns the retry request it re-emitted.
func runFallbackOnce(t *testing.T, models *engine.ModelRegistry, inboundEffort string) events.LLMRequest {
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
		Effort:        inboundEffort,
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}
	_, _ = bus.EmitVetoable("before:llm.request", origReq)

	errInfo := &events.ErrorInfo{
		SchemaVersion: events.ErrorInfoVersion,
		Source:        "nexus.llm.anthropic",
		Err:           fmt.Errorf("anthropic: API returned status 401: unauthorized"),
		Retryable:     false,
		RequestMeta:   origReq.Metadata,
	}
	result, err := bus.EmitVetoable("before:core.error", errInfo)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected the provider error to be vetoed by the fallback plugin")
	}
	if !gotRetry {
		t.Fatal("expected a retry llm.request to be re-emitted")
	}
	return retry
}

func TestFallback_ChainEntryEffortReachesRetryRequest(t *testing.T) {
	retry := runFallbackOnce(t, makeEffortRegistry(), "")

	if retry.Model != "gemini-2.5-pro" {
		t.Fatalf("retry model = %q, want the fallback entry's model", retry.Model)
	}
	if retry.Effort != "minimal" {
		t.Fatalf("retry effort = %q, want the fallback entry's %q (not the primary's)",
			retry.Effort, "minimal")
	}
}

func TestFallback_InboundEffortSurvivesUntouched(t *testing.T) {
	retry := runFallbackOnce(t, makeEffortRegistry(), "caller-set")

	if retry.Effort != "caller-set" {
		t.Fatalf("retry effort = %q, want the inbound %q to survive the chain entry",
			retry.Effort, "caller-set")
	}
}

func TestFallback_EntryWithoutEffortLeavesRequestUnset(t *testing.T) {
	retry := runFallbackOnce(t, makeTestRegistry(), "")

	if retry.Effort != "" {
		t.Fatalf("retry effort = %q, want empty when the entry sets none", retry.Effort)
	}
}
