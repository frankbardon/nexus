package openai

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestApiUsage_ParsesPromptTokensDetails verifies the apiUsage JSON tags match
// the OpenAI Chat Completions usage object — specifically the
// prompt_tokens_details.cached_tokens field that drives cache-aware billing.
func TestApiUsage_ParsesPromptTokensDetails(t *testing.T) {
	raw := []byte(`{
		"prompt_tokens": 1500,
		"completion_tokens": 200,
		"total_tokens": 1700,
		"prompt_tokens_details": {
			"cached_tokens": 1024,
			"audio_tokens": 0
		},
		"completion_tokens_details": {
			"reasoning_tokens": 96,
			"accepted_prediction_tokens": 32,
			"rejected_prediction_tokens": 8,
			"audio_tokens": 0
		}
	}`)

	var u apiUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshal apiUsage: %v", err)
	}

	if u.PromptTokens != 1500 {
		t.Errorf("PromptTokens: got %d, want 1500", u.PromptTokens)
	}
	if u.CompletionTokens != 200 {
		t.Errorf("CompletionTokens: got %d, want 200", u.CompletionTokens)
	}
	if u.TotalTokens != 1700 {
		t.Errorf("TotalTokens: got %d, want 1700", u.TotalTokens)
	}
	if u.PromptTokensDetails.CachedTokens != 1024 {
		t.Errorf("PromptTokensDetails.CachedTokens: got %d, want 1024", u.PromptTokensDetails.CachedTokens)
	}
	if u.CompletionTokensDetails.ReasoningTokens != 96 {
		t.Errorf("CompletionTokensDetails.ReasoningTokens: got %d, want 96", u.CompletionTokensDetails.ReasoningTokens)
	}
	if u.CompletionTokensDetails.AcceptedPredictionTokens != 32 {
		t.Errorf("CompletionTokensDetails.AcceptedPredictionTokens: got %d, want 32", u.CompletionTokensDetails.AcceptedPredictionTokens)
	}
}

// TestConvertAPIResponse_PopulatesCachedTokens verifies that convertAPIResponse
// surfaces prompt_tokens_details.cached_tokens onto events.Usage.CachedTokens.
func TestConvertAPIResponse_PopulatesCachedTokens(t *testing.T) {
	tbl := pricing.DefaultsFor(pricing.ProviderOpenAI)
	p := &Plugin{pricing: tbl}

	content := "hello"
	apiResp := apiResponse{
		ID:    "chatcmpl-test",
		Model: "gpt-4o",
		Choices: []apiChoice{
			{
				Index:        0,
				Message:      apiMessage{Role: "assistant", Content: &content},
				FinishReason: "stop",
			},
		},
	}
	apiResp.Usage.PromptTokens = 1500
	apiResp.Usage.CompletionTokens = 200
	apiResp.Usage.TotalTokens = 1700
	apiResp.Usage.PromptTokensDetails.CachedTokens = 1024

	resp := p.convertAPIResponse(apiResp)

	if resp.Usage.PromptTokens != 1500 {
		t.Errorf("PromptTokens: got %d, want 1500", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 200 {
		t.Errorf("CompletionTokens: got %d, want 200", resp.Usage.CompletionTokens)
	}
	if resp.Usage.CachedTokens != 1024 {
		t.Errorf("CachedTokens: got %d, want 1024", resp.Usage.CachedTokens)
	}

	rates, _ := tbl.Get("gpt-4o")
	wantCost := float64(1500-1024)/1_000_000*rates.InputPerMillion +
		float64(1024)/1_000_000*(rates.InputPerMillion*0.5) +
		float64(200)/1_000_000*rates.OutputPerMillion
	if math.Abs(resp.CostUSD-wantCost) > 1e-9 {
		t.Errorf("CostUSD: got %.10f, want %.10f", resp.CostUSD, wantCost)
	}
}

// TestCalculateCost_NoCache verifies the no-cache path matches the prior
// (input-only) cost calculation, so plain-input billing is preserved.
func TestCalculateCost_NoCache(t *testing.T) {
	rates := pricing.Rates{
		InputPerMillion:  2.50,
		OutputPerMillion: 10.0,
	}
	usage := events.Usage{
		PromptTokens:     1500,
		CompletionTokens: 200,
	}

	got := pricing.CalcOpenAI(usage, rates)
	want := float64(1500)/1_000_000*2.50 + float64(200)/1_000_000*10.0

	if math.Abs(got-want) > 1e-9 {
		t.Errorf("CalcOpenAI (no cache): got %.10f, want %.10f", got, want)
	}
}

// TestCalculateCost_HalfCacheDiscount verifies that a 50% cache hit yields
// roughly 75% of the plain-input cost (input portion only): half the prompt at
// full rate + half at 0.5× rate = 0.75× full rate.
func TestCalculateCost_HalfCacheDiscount(t *testing.T) {
	rates := pricing.Rates{
		InputPerMillion:  2.50,
		OutputPerMillion: 10.0,
	}

	cached := events.Usage{
		PromptTokens:     2000,
		CompletionTokens: 0,
		CachedTokens:     1000,
	}
	plain := events.Usage{
		PromptTokens:     2000,
		CompletionTokens: 0,
	}

	cachedCost := pricing.CalcOpenAI(cached, rates)
	plainCost := pricing.CalcOpenAI(plain, rates)

	want := plainCost * 0.75
	if math.Abs(cachedCost-want) > 1e-9 {
		t.Errorf("half-cache cost: got %.10f, want %.10f (plainCost=%.10f)", cachedCost, want, plainCost)
	}

	if cachedCost >= plainCost {
		t.Errorf("cached request must be cheaper: cached=%.6f plain=%.6f", cachedCost, plainCost)
	}
}

// TestCalculateCost_ExplicitCacheRate verifies that a configured
// cache_read_per_million override is honored instead of the derived 50%
// fallback.
func TestCalculateCost_ExplicitCacheRate(t *testing.T) {
	rates := pricing.Rates{
		InputPerMillion:     2.50,
		OutputPerMillion:    10.0,
		CacheReadPerMillion: 0.30,
	}
	usage := events.Usage{
		PromptTokens:     1_000_000,
		CompletionTokens: 0,
		CachedTokens:     1_000_000,
	}

	got := pricing.CalcOpenAI(usage, rates)
	want := 0.30
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("explicit cache rate cost: got %.10f, want %.10f", got, want)
	}
}

// TestCalculateCost_DefensiveNegativePlain verifies that an inconsistent
// payload where CachedTokens > PromptTokens does not produce a negative cost.
func TestCalculateCost_DefensiveNegativePlain(t *testing.T) {
	rates := pricing.Rates{
		InputPerMillion:  2.50,
		OutputPerMillion: 10.0,
	}
	usage := events.Usage{
		PromptTokens:     500,
		CompletionTokens: 0,
		CachedTokens:     1000,
	}

	got := pricing.CalcOpenAI(usage, rates)
	want := float64(1000) / 1_000_000 * (2.50 * 0.5)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("defensive cost: got %.10f, want %.10f", got, want)
	}
}

// TestParsePricingConfig_CacheReadKey verifies the cache_read_per_million YAML
// override key is parsed onto the merged price table.
func TestParsePricingConfig_CacheReadKey(t *testing.T) {
	cfg := map[string]any{
		"pricing": map[string]any{
			"gpt-4o": map[string]any{
				"input_per_million":      2.50,
				"output_per_million":     10.0,
				"cache_read_per_million": 1.25,
			},
		},
	}

	merged := parsePricingConfig(cfg)
	p, ok := merged.Get("gpt-4o")
	if !ok {
		t.Fatal("missing pricing entry for gpt-4o")
	}
	if p.InputPerMillion != 2.50 {
		t.Errorf("InputPerMillion: got %v, want 2.50", p.InputPerMillion)
	}
	if p.OutputPerMillion != 10.0 {
		t.Errorf("OutputPerMillion: got %v, want 10.0", p.OutputPerMillion)
	}
	if p.CacheReadPerMillion != 1.25 {
		t.Errorf("CacheReadPerMillion: got %v, want 1.25", p.CacheReadPerMillion)
	}
}
