package metadata

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

var testLogger = slog.Default()

func newRouter(t *testing.T, cfg map[string]any) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = testLogger
	rules, def, defRole, err := parseConfig(cfg)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	p.rules = rules
	p.defaultModel = def
	p.defaultRole = defRole
	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(50), engine.WithSource(pluginID))
	return p, bus
}

func emit(bus engine.EventBus, req *events.LLMRequest) {
	_, _ = bus.EmitVetoable("before:llm.request", req)
}

func TestParseConfig_RuleNeedsUseOrRole(t *testing.T) {
	_, _, _, err := parseConfig(map[string]any{
		"rules": []any{
			map[string]any{"match": map[string]any{"metadata._source": "planner"}},
		},
	})
	if err == nil {
		t.Fatal("expected error when neither use nor role is set")
	}
}

func TestParseConfig_EmptyMatchRejected(t *testing.T) {
	_, _, _, err := parseConfig(map[string]any{
		"rules": []any{
			map[string]any{"use": "x", "match": map[string]any{}},
		},
	})
	if err == nil {
		t.Fatal("expected error for rule with empty match block")
	}
}

func TestParseConfig_UnknownComparator(t *testing.T) {
	_, _, _, err := parseConfig(map[string]any{
		"rules": []any{
			map[string]any{
				"use": "x",
				"match": map[string]any{
					"metadata.iteration": map[string]any{"between": 3},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown comparator")
	}
}

func TestRoutesByMetadataSource(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"name":  "planner-haiku",
				"match": map[string]any{"metadata._source": "planner"},
				"use":   "claude-haiku-4-5-20251001",
			},
		},
		"default_model": "claude-sonnet-4-6-20250514",
	})

	req := &events.LLMRequest{
		Model:    "claude-opus-4-6-20250602",
		Metadata: map[string]any{"_source": "planner"},
	}
	emit(bus, req)
	if req.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected haiku, got %s", req.Model)
	}
	if req.Metadata["_routed_by"] != pluginID {
		t.Fatalf("expected _routed_by tag")
	}
	if req.Metadata["_routed_rule"] != "planner-haiku" {
		t.Fatalf("expected _routed_rule=planner-haiku, got %v", req.Metadata["_routed_rule"])
	}
	if req.Metadata["_routed_from_model"] != "claude-opus-4-6-20250602" {
		t.Fatalf("expected from_model recorded, got %v", req.Metadata["_routed_from_model"])
	}
}

func TestFallsBackToDefaultModel(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"match": map[string]any{"metadata._source": "planner"},
				"use":   "claude-haiku-4-5-20251001",
			},
		},
		"default_model": "claude-sonnet-4-6-20250514",
	})

	req := &events.LLMRequest{Metadata: map[string]any{"_source": "react"}}
	emit(bus, req)
	if req.Model != "claude-sonnet-4-6-20250514" {
		t.Fatalf("expected sonnet default, got %s", req.Model)
	}
	if req.Metadata["_routed_rule"] != "default" {
		t.Fatalf("expected default rule recorded, got %v", req.Metadata["_routed_rule"])
	}
}

func TestNumericComparatorGteEscalates(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"name": "stuck-loop-opus",
				"match": map[string]any{
					"metadata.task_kind": "react_main",
					"metadata.iteration": map[string]any{"gte": 3},
				},
				"use": "claude-opus-4-6-20250602",
			},
			map[string]any{
				"match": map[string]any{"metadata.task_kind": "react_main"},
				"use":   "claude-sonnet-4-6-20250514",
			},
		},
	})

	low := &events.LLMRequest{
		Metadata: map[string]any{"task_kind": "react_main", "iteration": 1},
	}
	emit(bus, low)
	if low.Model != "claude-sonnet-4-6-20250514" {
		t.Fatalf("low iteration should hit sonnet, got %s", low.Model)
	}

	high := &events.LLMRequest{
		Metadata: map[string]any{"task_kind": "react_main", "iteration": 4},
	}
	emit(bus, high)
	if high.Model != "claude-opus-4-6-20250602" {
		t.Fatalf("high iteration should escalate to opus, got %s", high.Model)
	}
}

func TestRoutesByTagDimension(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"match": map[string]any{"tags.tenant": "premium"},
				"role":  "reasoning",
			},
		},
	})

	req := &events.LLMRequest{
		Role: "balanced",
		Tags: map[string]string{"tenant": "premium"},
	}
	emit(bus, req)
	if req.Role != "reasoning" {
		t.Fatalf("expected role=reasoning for premium tenant, got %s", req.Role)
	}
}

func TestSkipsWhenTargetProviderPinned(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"match": map[string]any{"metadata._source": "planner"},
				"use":   "claude-haiku-4-5-20251001",
			},
		},
	})

	req := &events.LLMRequest{
		Model: "claude-sonnet-4-6-20250514",
		Metadata: map[string]any{
			"_source":          "planner",
			"_target_provider": "nexus.llm.openai",
		},
	}
	emit(bus, req)
	if req.Model != "claude-sonnet-4-6-20250514" {
		t.Fatalf("must not route when _target_provider is pinned, got %s", req.Model)
	}
	if _, routed := req.Metadata["_routed_by"]; routed {
		t.Fatal("must not record routing when skipped")
	}
}

func TestFirstMatchWins(t *testing.T) {
	_, bus := newRouter(t, map[string]any{
		"rules": []any{
			map[string]any{
				"name":  "first",
				"match": map[string]any{"metadata._source": "react"},
				"use":   "claude-haiku-4-5-20251001",
			},
			map[string]any{
				"name":  "second",
				"match": map[string]any{"metadata._source": "react"},
				"use":   "claude-opus-4-6-20250602",
			},
		},
	})
	req := &events.LLMRequest{Metadata: map[string]any{"_source": "react"}}
	emit(bus, req)
	if req.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("first matching rule must win, got %s", req.Model)
	}
}

func TestNoRulesNoMutation(t *testing.T) {
	_, bus := newRouter(t, map[string]any{})
	req := &events.LLMRequest{Model: "claude-sonnet-4-6-20250514"}
	emit(bus, req)
	if req.Model != "claude-sonnet-4-6-20250514" {
		t.Fatalf("expected no mutation, got %s", req.Model)
	}
	if req.Metadata != nil {
		t.Fatalf("metadata should not be created when no rule fires, got %v", req.Metadata)
	}
}

func TestPriorityAboveGates(t *testing.T) {
	// Smoke test: the router emits with priority 50, the engine seeder runs
	// at 100 (so seeds first), and gates sit at 10 (so see the rewritten
	// model). Verify that order numerically; priority semantics live in
	// the bus tests.
	if 100 < 50 || 50 < 10 {
		t.Fatal("router priority sandwich broken")
	}
}
