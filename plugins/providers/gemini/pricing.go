package gemini

import "github.com/frankbardon/nexus/pkg/events"

// modelPricing holds per-model token rates in USD per million tokens.
// CachedRatio multiplies InputPerMillion to derive the cost of cached prompt
// tokens (Google bills cached input at ~25% of standard input rate).
type modelPricing struct {
	InputPerMillion  float64
	OutputPerMillion float64
	CachedRatio      float64 // 0 disables; defaults to 0.25 if unset on lookup
}

// defaultPricing is the embedded fallback pricing table. Single tier; the
// 2.5-pro >200k tier (2.50 / 15.00) is not modeled here. Override via config
// when high-context billing matters.
var defaultPricing = map[string]modelPricing{
	"gemini-2.5-pro":        {InputPerMillion: 1.25, OutputPerMillion: 10.00, CachedRatio: 0.25},
	"gemini-2.5-flash":      {InputPerMillion: 0.30, OutputPerMillion: 2.50, CachedRatio: 0.25},
	"gemini-2.5-flash-lite": {InputPerMillion: 0.10, OutputPerMillion: 0.40, CachedRatio: 0.25},
	"gemini-2.0-flash":      {InputPerMillion: 0.10, OutputPerMillion: 0.40, CachedRatio: 0.25},
	"gemini-2.0-flash-lite": {InputPerMillion: 0.075, OutputPerMillion: 0.30, CachedRatio: 0.25},
	"gemini-1.5-pro":        {InputPerMillion: 1.25, OutputPerMillion: 5.00, CachedRatio: 0.25},
	"gemini-1.5-flash":      {InputPerMillion: 0.075, OutputPerMillion: 0.30, CachedRatio: 0.25},
	"gemini-1.5-flash-8b":   {InputPerMillion: 0.0375, OutputPerMillion: 0.15, CachedRatio: 0.25},
}

// calculateCost computes the USD cost for a single LLM call. CachedTokens are
// billed at the discounted rate; remaining prompt tokens at full input rate.
// ReasoningTokens (Gemini thoughtTokenCount) are billed at the output rate per
// Google's policy.
func calculateCost(usage events.Usage, rates modelPricing) float64 {
	cachedRatio := rates.CachedRatio
	if cachedRatio == 0 {
		cachedRatio = 0.25
	}

	cachedCost := float64(usage.CachedTokens) / 1_000_000 * rates.InputPerMillion * cachedRatio

	uncachedPrompt := usage.PromptTokens - usage.CachedTokens
	if uncachedPrompt < 0 {
		uncachedPrompt = 0
	}
	promptCost := float64(uncachedPrompt) / 1_000_000 * rates.InputPerMillion

	outputCost := float64(usage.CompletionTokens+usage.ReasoningTokens) / 1_000_000 * rates.OutputPerMillion

	return cachedCost + promptCost + outputCost
}

// parsePricingConfig merges embedded defaults with optional config overrides.
//
//	pricing:
//	  gemini-2.5-pro:
//	    input_per_million: 2.50    # >200k tier
//	    output_per_million: 15.0
//	    cached_ratio: 0.25
func parsePricingConfig(cfg map[string]any) map[string]modelPricing {
	merged := make(map[string]modelPricing, len(defaultPricing))
	for k, v := range defaultPricing {
		merged[k] = v
	}

	raw, ok := cfg["pricing"].(map[string]any)
	if !ok {
		return merged
	}

	for model, val := range raw {
		entry, ok := val.(map[string]any)
		if !ok {
			continue
		}
		p := merged[model]
		if v, ok := entry["input_per_million"].(float64); ok {
			p.InputPerMillion = v
		}
		if v, ok := entry["output_per_million"].(float64); ok {
			p.OutputPerMillion = v
		}
		if v, ok := entry["cached_ratio"].(float64); ok {
			p.CachedRatio = v
		}
		merged[model] = p
	}

	return merged
}
