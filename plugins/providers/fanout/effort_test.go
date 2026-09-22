package fanout

import (
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// makeEffortFanoutPlugin builds a fanout coordinator over a two-provider fanout
// role whose entries carry distinct per-entry effort values.
func makeEffortFanoutPlugin(t *testing.T) (*Plugin, *mockBus) {
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
					"effort":   "high",
				},
				map[string]any{
					"provider": "nexus.llm.gemini",
					"model":    "gemini-2.5-pro",
					"effort":   "minimal",
				},
			},
		},
	})
	return p, bus
}

// fanRequests pulls the dispatched llm.request payloads out of the mock bus,
// keyed by the provider each was targeted at.
func fanRequests(t *testing.T, bus *mockBus) map[string]events.LLMRequest {
	t.Helper()

	out := make(map[string]events.LLMRequest)
	for _, ev := range bus.emitted() {
		if ev.Type != "llm.request" {
			continue
		}
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			t.Fatalf("expected LLMRequest payload, got %T", ev.Payload)
		}
		target, _ := req.Metadata["_target_provider"].(string)
		out[target] = req
	}
	return out
}

func TestFanout_PerEntryEffortReachesEachFanRequest(t *testing.T) {
	p, bus := makeEffortFanoutPlugin(t)

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
	if got := reqs["nexus.llm.anthropic"].Effort; got != "high" {
		t.Fatalf("anthropic fan request effort = %q, want %q", got, "high")
	}
	if got := reqs["nexus.llm.gemini"].Effort; got != "minimal" {
		t.Fatalf("gemini fan request effort = %q, want %q", got, "minimal")
	}
}

func TestFanout_InboundEffortSurvivesUntouched(t *testing.T) {
	p, bus := makeEffortFanoutPlugin(t)

	vp := &engine.VetoablePayload{Original: &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "wide",
		Effort:        "caller-set",
		Messages:      []events.Message{{Role: "user", Content: "hello"}},
	}}
	p.handleBeforeRequest(engine.Event[any]{Type: "before:llm.request", Payload: vp})

	reqs := fanRequests(t, bus)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 fan requests, got %d", len(reqs))
	}
	for provider, req := range reqs {
		if req.Effort != "caller-set" {
			t.Fatalf("%s fan request effort = %q, want the inbound %q to survive",
				provider, req.Effort, "caller-set")
		}
	}
}

func TestFanout_EntryWithoutEffortLeavesRequestUnset(t *testing.T) {
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

	reqs := fanRequests(t, bus)
	if got := reqs["nexus.llm.anthropic"].Effort; got != "" {
		t.Fatalf("fan request effort = %q, want empty when the entry sets none", got)
	}
}
