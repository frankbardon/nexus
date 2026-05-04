package pricing

import (
	"math"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func approx(t *testing.T, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("got %.6f want %.6f (tol %.6f)", got, want, tol)
	}
}

func TestDefaultsForKnownProviders(t *testing.T) {
	for _, p := range []string{ProviderAnthropic, ProviderOpenAI, ProviderGemini} {
		tbl := DefaultsFor(p)
		if tbl == nil {
			t.Fatalf("nil table for %s", p)
		}
		if len(tbl.Models()) == 0 {
			t.Fatalf("empty defaults for %s", p)
		}
	}
}

func TestDefaultsForUnknownProviderEmpty(t *testing.T) {
	tbl := DefaultsFor("unknown")
	if tbl == nil {
		t.Fatal("nil table")
	}
	if len(tbl.Models()) != 0 {
		t.Fatalf("expected empty, got %d models", len(tbl.Models()))
	}
	if got := tbl.Calc("gpt-4o", events.Usage{PromptTokens: 1000, CompletionTokens: 1000}); got != 0 {
		t.Fatalf("expected 0 cost for unknown provider, got %f", got)
	}
}

func TestCalcAnthropicPlainInputOnly(t *testing.T) {
	r := Rates{InputPerMillion: 3.0, OutputPerMillion: 15.0}
	got := CalcAnthropic(events.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}, r)
	approx(t, got, 18.0, 0.0001)
}

func TestCalcAnthropicCacheRatesDerived(t *testing.T) {
	r := Rates{InputPerMillion: 3.0, OutputPerMillion: 15.0}
	usage := events.Usage{
		PromptTokens:     500_000,
		CompletionTokens: 200_000,
		CachedTokens:     1_000_000,
		CacheWriteTokens: 100_000,
	}
	got := CalcAnthropic(usage, r)
	want := 500_000.0/1_000_000*3.0 +
		100_000.0/1_000_000*(3.0*1.25) +
		1_000_000.0/1_000_000*(3.0*0.10) +
		200_000.0/1_000_000*15.0
	approx(t, got, want, 0.0001)
}

func TestCalcAnthropicExplicitCacheRates(t *testing.T) {
	r := Rates{
		InputPerMillion:        3.0,
		OutputPerMillion:       15.0,
		CacheReadPerMillion:    0.50,
		CacheWrite5mPerMillion: 5.0,
	}
	usage := events.Usage{CachedTokens: 1_000_000, CacheWriteTokens: 1_000_000}
	got := CalcAnthropic(usage, r)
	approx(t, got, 0.5+5.0, 0.0001)
}

func TestCalcOpenAICachedDerived(t *testing.T) {
	r := Rates{InputPerMillion: 2.50, OutputPerMillion: 10.0}
	usage := events.Usage{PromptTokens: 1_000_000, CachedTokens: 400_000, CompletionTokens: 100_000}
	got := CalcOpenAI(usage, r)
	want := 600_000.0/1_000_000*2.50 + 400_000.0/1_000_000*1.25 + 100_000.0/1_000_000*10.0
	approx(t, got, want, 0.0001)
}

func TestCalcOpenAICachedExplicit(t *testing.T) {
	r := Rates{InputPerMillion: 2.50, OutputPerMillion: 10.0, CacheReadPerMillion: 1.0}
	usage := events.Usage{PromptTokens: 1_000_000, CachedTokens: 1_000_000}
	got := CalcOpenAI(usage, r)
	approx(t, got, 1.0, 0.0001)
}

func TestCalcGeminiCachedAndReasoning(t *testing.T) {
	r := Rates{InputPerMillion: 1.25, OutputPerMillion: 10.00}
	usage := events.Usage{
		PromptTokens:     1_000_000,
		CachedTokens:     500_000,
		CompletionTokens: 500_000,
		ReasoningTokens:  500_000,
	}
	got := CalcGemini(usage, r)
	want := 500_000.0/1_000_000*1.25*0.25 + // cached
		500_000.0/1_000_000*1.25 + // plain input
		1_000_000.0/1_000_000*10.0 // output + reasoning
	approx(t, got, want, 0.0001)
}

func TestMergeOverridesAddsAndUpdates(t *testing.T) {
	tbl := DefaultsFor(ProviderAnthropic)
	tbl.Merge(map[string]any{
		"claude-haiku-4-5-20251001": map[string]any{
			"input_per_million":  1.0,
			"output_per_million": 5.0,
		},
		"claude-future-1-0": map[string]any{
			"input_per_million":  2.0,
			"output_per_million": 10.0,
		},
	})
	r, ok := tbl.Get("claude-haiku-4-5-20251001")
	if !ok || r.InputPerMillion != 1.0 || r.OutputPerMillion != 5.0 {
		t.Fatalf("override not applied: %+v ok=%v", r, ok)
	}
	r2, ok := tbl.Get("claude-future-1-0")
	if !ok || r2.InputPerMillion != 2.0 {
		t.Fatalf("new model not added: %+v ok=%v", r2, ok)
	}
}

func TestMergeIgnoresMalformed(t *testing.T) {
	tbl := DefaultsFor(ProviderOpenAI)
	tbl.Merge(map[string]any{
		"gpt-4o":      "not-a-map",
		"gpt-4o-mini": map[string]any{"unknown_field": 9.99},
	})
	r, _ := tbl.Get("gpt-4o-mini")
	if r.InputPerMillion != 0.15 {
		t.Fatalf("merge corrupted defaults: %+v", r)
	}
}

func TestCheapestModel(t *testing.T) {
	tbl := DefaultsFor(ProviderAnthropic)
	got := tbl.CheapestModel([]string{
		"claude-opus-4-6-20250602",
		"claude-haiku-4-5-20251001",
		"claude-sonnet-4-6-20250514",
	})
	if got != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected haiku as cheapest, got %s", got)
	}
}

func TestCheapestModelSkipsUnknown(t *testing.T) {
	tbl := DefaultsFor(ProviderAnthropic)
	got := tbl.CheapestModel([]string{"unknown", "claude-haiku-4-5-20251001"})
	if got != "claude-haiku-4-5-20251001" {
		t.Fatalf("got %s", got)
	}
}

func TestCheapestModelEmpty(t *testing.T) {
	tbl := DefaultsFor(ProviderAnthropic)
	if got := tbl.CheapestModel(nil); got != "" {
		t.Fatalf("expected empty, got %s", got)
	}
	if got := tbl.CheapestModel([]string{"unknown-1", "unknown-2"}); got != "" {
		t.Fatalf("expected empty for all-unknown, got %s", got)
	}
}

func TestTableCalcDispatch(t *testing.T) {
	tbl := DefaultsFor(ProviderAnthropic)
	usage := events.Usage{PromptTokens: 1_000_000}
	got := tbl.Calc("claude-haiku-4-5-20251001", usage)
	approx(t, got, 0.80, 0.0001)
	if got := tbl.Calc("not-a-real-model", usage); got != 0 {
		t.Fatalf("expected 0 for unknown model, got %f", got)
	}
}
