package fanout

import (
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// makeOverridesFanoutPlugin builds a fanout coordinator whose two legs carry
// different native provider blocks, so a test can tell a non-first leg's own
// block apart from the role's first one.
func makeOverridesFanoutPlugin(t *testing.T) (*Plugin, *mockBus) {
	t.Helper()

	bus := &mockBus{}
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.cfg = config{
		Strategy:       StrategyAll,
		DeadlineMillis: 60000,
		deadline:       60 * time.Second,
	}
	p.models = engine.NewModelRegistry(map[string]any{
		"default": "wide",
		"wide": map[string]any{
			"fanout": true,
			"providers": []any{
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
					"temperature": 0,
					"thinking": map[string]any{
						"mode":          "budget",
						"budget_tokens": 2048,
					},
				},
			},
		},
	})
	return p, bus
}

func TestFanout_PerEntryBlocksReachEachFanRequest(t *testing.T) {
	p, bus := makeOverridesFanoutPlugin(t)

	vp := &engine.VetoablePayload{Original: &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "wide",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}}
	p.handleBeforeRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	if !vp.Veto.Vetoed {
		t.Fatal("expected the original request to be vetoed and fanned out")
	}

	reqs := fanRequests(t, bus)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 fan requests, got %d", len(reqs))
	}

	first := reqs["nexus.llm.anthropic"]
	if !first.Overrides.Stamped {
		t.Fatal("fan requests must be stamped so the provider skips the registry")
	}
	if got := first.Overrides.Thinking["mode"]; got != "adaptive" {
		t.Fatalf("anthropic leg thinking.mode = %v, want %q", got, "adaptive")
	}
	if got := first.Overrides.API; got != "messages" {
		t.Fatalf("anthropic leg api = %q, want %q", got, "messages")
	}

	second := reqs["nexus.llm.gemini"]
	if got := second.Overrides.Thinking["mode"]; got != "budget" {
		t.Fatalf("gemini leg thinking.mode = %v, want its own %q rather than the first leg's %q",
			got, "budget", "adaptive")
	}
	if got := second.Overrides.Thinking["budget_tokens"]; got != 2048 {
		t.Fatalf("gemini leg thinking.budget_tokens = %v, want 2048", got)
	}
	if second.Temperature == nil || *second.Temperature != 0 {
		t.Fatalf("gemini leg temperature = %v, want its own explicit 0", second.Temperature)
	}
	// Neither the first leg's cache nor its api may leak onto a leg that sets none.
	if second.Overrides.Cache != nil {
		t.Fatalf("gemini leg cache = %v, want nil — only the first leg sets one", second.Overrides.Cache)
	}
	if second.Overrides.API != "" {
		t.Fatalf("gemini leg api = %q, want empty — only the first leg sets one", second.Overrides.API)
	}
	if first.Temperature != nil {
		t.Fatalf("anthropic leg temperature = %v, want nil — only the second leg sets one", first.Temperature)
	}
}

func TestFanout_InboundOverridesSurviveUntouched(t *testing.T) {
	p, bus := makeOverridesFanoutPlugin(t)

	vp := &engine.VetoablePayload{Original: &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "wide",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
		Overrides: events.ModelOverrides{
			API:      "caller-api",
			Thinking: map[string]any{"mode": "caller"},
		},
	}}
	p.handleBeforeRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	reqs := fanRequests(t, bus)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 fan requests, got %d", len(reqs))
	}
	for provider, req := range reqs {
		if req.Overrides.API != "caller-api" {
			t.Fatalf("%s leg api = %q, want the inbound value to survive", provider, req.Overrides.API)
		}
		if got := req.Overrides.Thinking["mode"]; got != "caller" {
			t.Fatalf("%s leg thinking.mode = %v, want the inbound block to survive", provider, got)
		}
	}
	// An axis the caller left alone is still filled per leg.
	if c := reqs["nexus.llm.anthropic"].Overrides.Cache; c == nil || c["enabled"] != true {
		t.Fatalf("anthropic leg cache = %v, want the entry to fill the gap", c)
	}
}

func TestFanout_EntryWithoutBlocksLeavesRequestUnset(t *testing.T) {
	bus := &mockBus{}
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.cfg = config{Strategy: StrategyAll, DeadlineMillis: 60000, deadline: 60 * time.Second}
	p.models = engine.NewModelRegistry(map[string]any{
		"wide": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{"provider": "nexus.llm.anthropic", "model": "claude-sonnet-4-20250514"},
			},
		},
	})

	vp := &engine.VetoablePayload{Original: &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "wide",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}}
	p.handleBeforeRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	req := fanRequests(t, bus)["nexus.llm.anthropic"]
	if req.Overrides.Thinking != nil || req.Overrides.Reasoning != nil ||
		req.Overrides.Cache != nil || req.Overrides.Retry != nil || req.Overrides.API != "" {
		t.Fatalf("fan request overrides = %+v, want every axis unset when the entry sets none",
			req.Overrides)
	}
	if !req.Overrides.Stamped {
		t.Fatal("a stamp with nothing to fill is still a stamp")
	}
}
