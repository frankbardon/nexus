package anthropic

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// TestApiUsage_ParsesCacheFields verifies the apiUsage JSON tags match the
// Anthropic Messages API usage object (cache_creation_input_tokens and
// cache_read_input_tokens).
func TestApiUsage_ParsesCacheFields(t *testing.T) {
	raw := []byte(`{
		"input_tokens": 142,
		"output_tokens": 350,
		"cache_creation_input_tokens": 1024,
		"cache_read_input_tokens": 4096
	}`)

	var u apiUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshal apiUsage: %v", err)
	}

	if u.InputTokens != 142 {
		t.Errorf("InputTokens: got %d, want 142", u.InputTokens)
	}
	if u.OutputTokens != 350 {
		t.Errorf("OutputTokens: got %d, want 350", u.OutputTokens)
	}
	if u.CacheCreationInputTokens != 1024 {
		t.Errorf("CacheCreationInputTokens: got %d, want 1024", u.CacheCreationInputTokens)
	}
	if u.CacheReadInputTokens != 4096 {
		t.Errorf("CacheReadInputTokens: got %d, want 4096", u.CacheReadInputTokens)
	}
}

// TestConvertAPIResponse_PopulatesCacheUsage verifies that convertAPIResponse
// surfaces cache-write and cache-read tokens onto events.Usage.
func TestConvertAPIResponse_PopulatesCacheUsage(t *testing.T) {
	p := &Plugin{pricing: defaultPricing}

	apiResp := apiResponse{
		ID:    "msg_test",
		Model: "claude-sonnet-4-6-20250514",
		Content: []apiContentBlock{
			{Type: "text", Text: "hello"},
		},
		StopReason: "end_turn",
		Usage: apiUsage{
			InputTokens:              1000,
			OutputTokens:             350,
			CacheCreationInputTokens: 1024,
			CacheReadInputTokens:     4096,
		},
	}

	resp := p.convertAPIResponse(apiResp)

	if resp.Usage.PromptTokens != 1000 {
		t.Errorf("PromptTokens: got %d, want 1000", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 350 {
		t.Errorf("CompletionTokens: got %d, want 350", resp.Usage.CompletionTokens)
	}
	if resp.Usage.CachedTokens != 4096 {
		t.Errorf("CachedTokens: got %d, want 4096", resp.Usage.CachedTokens)
	}
	if resp.Usage.CacheWriteTokens != 1024 {
		t.Errorf("CacheWriteTokens: got %d, want 1024", resp.Usage.CacheWriteTokens)
	}
	wantTotal := 1000 + 350 + 4096 + 1024
	if resp.Usage.TotalTokens != wantTotal {
		t.Errorf("TotalTokens: got %d, want %d", resp.Usage.TotalTokens, wantTotal)
	}

	// Cost should be non-zero now that cache fields contribute.
	if resp.CostUSD <= 0 {
		t.Errorf("CostUSD: got %v, want > 0", resp.CostUSD)
	}
}

// TestCalculateCost_CacheRates verifies the cost formula bills each token
// category at its respective rate. Uses Sonnet-4 default rates with derived
// cache rates (read = input × 0.10, write_5m = input × 1.25).
func TestCalculateCost_CacheRates(t *testing.T) {
	rates := defaultPricing["claude-sonnet-4-6-20250514"]
	usage := events.Usage{
		PromptTokens:     1000, // plain (cache-miss) input
		CompletionTokens: 350,  // output
		CachedTokens:     4096, // cache read
		CacheWriteTokens: 1024, // cache write (5m)
	}

	got := calculateCost(usage, rates)

	// Expected:
	//   plain  = 1000  / 1e6 * 3.0       = 0.003
	//   write  = 1024  / 1e6 * 3.0*1.25  = 0.00384
	//   read   = 4096  / 1e6 * 3.0*0.10  = 0.0012288
	//   output = 350   / 1e6 * 15.0      = 0.00525
	want := 0.003 + 0.00384 + 0.0012288 + 0.00525

	if math.Abs(got-want) > 1e-9 {
		t.Errorf("calculateCost: got %.10f, want %.10f", got, want)
	}
}

// TestCalculateCost_CacheCheaperThanPlain verifies that a cache-heavy request
// is priced strictly lower than the equivalent plain-input request — this is
// the whole point of caching, so guard it explicitly.
func TestCalculateCost_CacheCheaperThanPlain(t *testing.T) {
	rates := defaultPricing["claude-sonnet-4-6-20250514"]

	cached := events.Usage{
		PromptTokens:     100,
		CompletionTokens: 200,
		CachedTokens:     5000, // mostly cache hits
		CacheWriteTokens: 0,
	}
	plain := events.Usage{
		PromptTokens:     5100, // same total input, no cache
		CompletionTokens: 200,
	}

	cachedCost := calculateCost(cached, rates)
	plainCost := calculateCost(plain, rates)

	if cachedCost >= plainCost {
		t.Errorf("expected cached request cheaper than plain: cached=%.6f plain=%.6f", cachedCost, plainCost)
	}
}

// TestCalculateCost_ExplicitCacheRates verifies that configured cache rates
// are honored over the derived fallbacks.
func TestCalculateCost_ExplicitCacheRates(t *testing.T) {
	rates := modelPricing{
		InputPerMillion:        3.0,
		OutputPerMillion:       15.0,
		CacheReadPerMillion:    0.50, // override (would be 0.30 derived)
		CacheWrite5mPerMillion: 4.00, // override (would be 3.75 derived)
	}
	usage := events.Usage{
		PromptTokens:     0,
		CompletionTokens: 0,
		CachedTokens:     1_000_000,
		CacheWriteTokens: 1_000_000,
	}

	got := calculateCost(usage, rates)
	want := 0.50 + 4.00 // each 1M tokens * its rate
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("explicit-rates cost: got %.6f, want %.6f", got, want)
	}
}

// TestParsePricingConfig_CacheKeys verifies the YAML override keys for cache
// rates are parsed onto modelPricing.
func TestParsePricingConfig_CacheKeys(t *testing.T) {
	cfg := map[string]any{
		"pricing": map[string]any{
			"claude-sonnet-4-6-20250514": map[string]any{
				"input_per_million":          5.0,
				"output_per_million":         20.0,
				"cache_read_per_million":     0.42,
				"cache_write_5m_per_million": 6.25,
				"cache_write_1h_per_million": 10.0,
			},
		},
	}

	merged := parsePricingConfig(cfg)
	p, ok := merged["claude-sonnet-4-6-20250514"]
	if !ok {
		t.Fatal("missing pricing entry for claude-sonnet-4-6-20250514")
	}
	if p.InputPerMillion != 5.0 {
		t.Errorf("InputPerMillion: got %v, want 5.0", p.InputPerMillion)
	}
	if p.OutputPerMillion != 20.0 {
		t.Errorf("OutputPerMillion: got %v, want 20.0", p.OutputPerMillion)
	}
	if p.CacheReadPerMillion != 0.42 {
		t.Errorf("CacheReadPerMillion: got %v, want 0.42", p.CacheReadPerMillion)
	}
	if p.CacheWrite5mPerMillion != 6.25 {
		t.Errorf("CacheWrite5mPerMillion: got %v, want 6.25", p.CacheWrite5mPerMillion)
	}
	if p.CacheWrite1hPerMillion != 10.0 {
		t.Errorf("CacheWrite1hPerMillion: got %v, want 10.0", p.CacheWrite1hPerMillion)
	}
}
