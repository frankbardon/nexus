package tokenbudget

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

var testLogger = slog.Default()

func newGate(t *testing.T, cfg map[string]any) (*Plugin, engine.EventBus) {
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
	return p, bus
}

func TestParseCeilings_LegacyMaxTokens(t *testing.T) {
	cs, err := parseCeilings(map[string]any{"max_tokens": 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Dimension != dimSession || cs[0].MaxTotal != 1000 {
		t.Fatalf("legacy max_tokens not honored: %+v", cs)
	}
}

func TestParseCeilings_RejectsUnknownDimension(t *testing.T) {
	_, err := parseCeilings(map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "bogus", "max_total_tokens": 1},
		},
	})
	if err == nil {
		t.Fatal("expected error on unknown dimension")
	}
}

func TestParseCeilings_RejectsEmptyLimit(t *testing.T) {
	_, err := parseCeilings(map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "session"},
		},
	})
	if err == nil {
		t.Fatal("expected error when no limit field is set")
	}
}

func TestParseCeilings_USDPerDayImpliesDayWindow(t *testing.T) {
	cs, err := parseCeilings(map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":       "tenant",
				"max_usd_per_day": 5.0,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Window != windowDay || cs[0].MaxUSD != 5.0 {
		t.Fatalf("expected day window with $5 cap, got %+v", cs[0])
	}
}

func TestSessionCeiling_VetoesAtTotalLimit(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "session",
				"max_total_tokens": 100,
				"on_exceed":        "block",
			},
		},
	})

	// Spend up to the cap.
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 100}})

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if !veto.Vetoed {
		t.Fatal("expected veto when session total at cap")
	}
}

func TestSessionCeiling_NoVetoBelowLimit(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "session",
				"max_total_tokens": 100,
			},
		},
	})
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 50}})
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if veto.Vetoed {
		t.Fatal("must not veto below cap")
	}
}

func TestSourcePluginCeiling_OnlyAffectsMatchedBucket(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "source_plugin",
				"match":            "tools.web",
				"max_total_tokens": 50,
			},
		},
	})

	// Charge tools.web up to limit, plus an unrelated bucket.
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 50},
		Tags: map[string]string{"source_plugin": "tools.web"},
	})
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 9999},
		Tags: map[string]string{"source_plugin": "tools.shell"},
	})

	// Request from tools.web → veto.
	webReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tags: map[string]string{"source_plugin": "tools.web"}}
	veto, _ := bus.EmitVetoable("before:llm.request", webReq)
	if !veto.Vetoed {
		t.Fatal("tools.web request must veto")
	}

	// Request from tools.shell → must pass (different bucket).
	shellReq := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tags: map[string]string{"source_plugin": "tools.shell"}}
	veto, _ = bus.EmitVetoable("before:llm.request", shellReq)
	if veto.Vetoed {
		t.Fatal("tools.shell request must not veto under tools.web ceiling")
	}
}

func TestDowngradeAction_RewritesModel(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "session",
				"max_total_tokens": 10,
				"on_exceed":        "downgrade-model",
			},
		},
		"downgrade_candidates": []any{
			"claude-opus-4-7",
			"claude-haiku-4-5-20251001",
			"claude-sonnet-4-6",
		},
	})

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 10}})
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-opus-4-7"}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if veto.Vetoed {
		t.Fatal("downgrade-model must not veto")
	}
	if req.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected downgrade to haiku, got %s", req.Model)
	}
	if req.Metadata["_downgraded_by"] != pluginID {
		t.Fatalf("expected _downgraded_by tag")
	}
	if req.Metadata["_downgraded_from"] != "claude-opus-4-7" {
		t.Fatalf("expected previous model recorded, got %v", req.Metadata["_downgraded_from"])
	}
}

func TestDowngradeAction_NoCandidatesLeavesAlone(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "session",
				"max_total_tokens": 1,
				"on_exceed":        "downgrade-model",
			},
		},
	})
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 1}})
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Model: "claude-opus-4-7"}
	bus.EmitVetoable("before:llm.request", req)
	if req.Model != "claude-opus-4-7" {
		t.Fatalf("model should be untouched without candidates, got %s", req.Model)
	}
}

func TestUSDCeiling(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension": "session",
				"max_usd":   0.05,
			},
		},
	})
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, CostUSD: 0.06})
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if !veto.Vetoed {
		t.Fatal("expected veto when USD spend exceeds cap")
	}
}

func TestWarnAction_DoesNotVeto(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{
				"dimension":        "session",
				"max_total_tokens": 10,
				"on_exceed":        "warn",
			},
		},
	})
	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{TotalTokens: 100}})
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	veto, _ := bus.EmitVetoable("before:llm.request", req)
	if veto.Vetoed {
		t.Fatal("warn must not veto")
	}
}

func TestExceededCheckBoundaries(t *testing.T) {
	c := ceiling{Dimension: dimSession, MaxTotal: 100}
	if ex, _ := c.exceeded(usageTotals{TotalTokens: 99}); ex {
		t.Fatal("99 should not exceed 100")
	}
	if ex, _ := c.exceeded(usageTotals{TotalTokens: 100}); !ex {
		t.Fatal("100 should exceed 100 (>= semantics)")
	}
}

func TestMergedPricing_SpansAllProviders(t *testing.T) {
	tbl := mergedPricing(nil)
	for _, m := range []string{"claude-haiku-4-5-20251001", "gpt-4o-mini", "gemini-2.5-flash"} {
		if _, ok := tbl.Get(m); !ok {
			t.Errorf("merged table missing %s", m)
		}
	}
}
