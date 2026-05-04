// Package pricing centralizes per-model token pricing for all LLM providers.
//
// Each provider previously embedded its own pricing table and cost calculator.
// That worked but made the cost CLI, multi-dimensional budget gate, and
// router (idea 09) reach into provider-internals to compare models. This
// package is the single source of truth: defaults shipped per provider,
// merged with optional config overrides, plus per-provider Calc dispatch
// that keeps the distinct cache semantics each vendor bills under.
package pricing

import (
	"maps"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// Provider identifiers used by Calc to pick cache semantics.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderGemini    = "gemini"
)

// Rates holds per-model token rates expressed in USD per million tokens.
//
// Not every field applies to every provider:
//   - Anthropic uses Input/Output + the explicit cache lanes (read, write 5m,
//     write 1h). Zero cache lanes derive from InputPerMillion at provider
//     defaults (read 0.10×, write 5m 1.25×, write 1h 2.0×).
//   - OpenAI uses Input/Output + CacheReadPerMillion. Zero CacheRead derives as
//     0.5× InputPerMillion (the auto-cache discount).
//   - Gemini uses Input/Output + CachedRatio. Zero ratio defaults to 0.25.
//     Reasoning tokens are billed at the output rate.
//
// CalcAnthropic / CalcOpenAI / CalcGemini encode each provider's billing
// rules; callers pick the right one or use Table.Calc.
type Rates struct {
	InputPerMillion        float64
	OutputPerMillion       float64
	CacheReadPerMillion    float64
	CacheWrite5mPerMillion float64
	CacheWrite1hPerMillion float64
	CachedRatio            float64
}

// Table is a per-provider price table. Construct via DefaultsFor and call
// Merge to layer config overrides on top.
type Table struct {
	Provider string
	rates    map[string]Rates
}

// NewTable returns an empty table for the given provider.
func NewTable(provider string) *Table {
	return &Table{Provider: strings.ToLower(provider), rates: make(map[string]Rates)}
}

// DefaultsFor returns the embedded default price table for the given provider.
// Unknown providers return an empty table — callers should still receive a
// usable (zero-cost) result rather than a nil pointer.
func DefaultsFor(provider string) *Table {
	switch strings.ToLower(provider) {
	case ProviderAnthropic:
		return &Table{Provider: ProviderAnthropic, rates: cloneRates(anthropicDefaults)}
	case ProviderOpenAI:
		return &Table{Provider: ProviderOpenAI, rates: cloneRates(openaiDefaults)}
	case ProviderGemini:
		return &Table{Provider: ProviderGemini, rates: cloneRates(geminiDefaults)}
	default:
		return NewTable(provider)
	}
}

// Get returns the rates for a model. The bool reports whether the model was
// found in the table (defaults + overrides).
func (t *Table) Get(model string) (Rates, bool) {
	if t == nil {
		return Rates{}, false
	}
	r, ok := t.rates[model]
	return r, ok
}

// Models returns the model IDs known to the table, in unspecified order.
func (t *Table) Models() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.rates))
	for k := range t.rates {
		out = append(out, k)
	}
	return out
}

// Set installs rates for a single model. Used by Merge and by tests.
func (t *Table) Set(model string, rates Rates) {
	if t.rates == nil {
		t.rates = make(map[string]Rates)
	}
	t.rates[model] = rates
}

// Merge layers user-supplied overrides on top of the existing entries.
//
// Accepted YAML shape (per-model fields are all optional):
//
//	pricing:
//	  claude-sonnet-4-6-20250514:
//	    input_per_million: 3.0
//	    output_per_million: 15.0
//	    cache_read_per_million: 0.30
//	    cache_write_5m_per_million: 3.75
//	    cache_write_1h_per_million: 6.0
//	  gemini-2.5-pro:
//	    cached_ratio: 0.25
//
// Unknown keys are ignored. Nil overrides is a no-op.
func (t *Table) Merge(overrides map[string]any) {
	if t == nil || len(overrides) == 0 {
		return
	}
	for model, val := range overrides {
		entry, ok := val.(map[string]any)
		if !ok {
			continue
		}
		r := t.rates[model]
		if v, ok := numericFloat(entry, "input_per_million"); ok {
			r.InputPerMillion = v
		}
		if v, ok := numericFloat(entry, "output_per_million"); ok {
			r.OutputPerMillion = v
		}
		if v, ok := numericFloat(entry, "cache_read_per_million"); ok {
			r.CacheReadPerMillion = v
		}
		if v, ok := numericFloat(entry, "cache_write_5m_per_million"); ok {
			r.CacheWrite5mPerMillion = v
		}
		if v, ok := numericFloat(entry, "cache_write_1h_per_million"); ok {
			r.CacheWrite1hPerMillion = v
		}
		if v, ok := numericFloat(entry, "cached_ratio"); ok {
			r.CachedRatio = v
		}
		t.rates[model] = r
	}
}

// Calc dispatches to the per-provider calculator for the table's provider.
// Unknown providers return 0 (no rates table = no cost). Unknown models
// return 0 by the same logic.
func (t *Table) Calc(model string, usage events.Usage) float64 {
	if t == nil {
		return 0
	}
	rates, ok := t.rates[model]
	if !ok {
		return 0
	}
	switch t.Provider {
	case ProviderAnthropic:
		return CalcAnthropic(usage, rates)
	case ProviderOpenAI:
		return CalcOpenAI(usage, rates)
	case ProviderGemini:
		return CalcGemini(usage, rates)
	default:
		return 0
	}
}

// CheapestModel returns the model id with the lowest blended input+output
// rate among the candidates that exist in the table. Used by the budget
// gate's downgrade-model action and by routers.
//
// blended = InputPerMillion + OutputPerMillion (a coarse proxy that ranks
// well enough for routing without requiring an actual usage profile).
//
// Returns "" when no candidate is known to the table.
func (t *Table) CheapestModel(candidates []string) string {
	if t == nil || len(candidates) == 0 {
		return ""
	}
	var best string
	bestScore := 0.0
	for _, m := range candidates {
		r, ok := t.rates[m]
		if !ok {
			continue
		}
		score := r.InputPerMillion + r.OutputPerMillion
		if best == "" || score < bestScore {
			best = m
			bestScore = score
		}
	}
	return best
}

// CalcAnthropic implements Anthropic's billing model.
//
// Anthropic's reported `input_tokens` excludes both cache-creation and
// cache-read portions, so PromptTokens is the cache-miss (plain input)
// count and CachedTokens / CacheWriteTokens are billed separately.
//
// CacheWriteTokens are charged at the 5-minute-TTL rate; per-request TTL
// selection (idea 05 prompt caching plan) routes 1h writes via the explicit
// CacheWrite1hPerMillion rate when callers split the counts. Today no
// caller split is in place, so all writes hit the 5m lane.
func CalcAnthropic(usage events.Usage, r Rates) float64 {
	cacheReadRate := r.CacheReadPerMillion
	if cacheReadRate == 0 {
		cacheReadRate = r.InputPerMillion * 0.10
	}
	cacheWriteRate := r.CacheWrite5mPerMillion
	if cacheWriteRate == 0 {
		cacheWriteRate = r.InputPerMillion * 1.25
	}
	return float64(usage.PromptTokens)/1_000_000*r.InputPerMillion +
		float64(usage.CacheWriteTokens)/1_000_000*cacheWriteRate +
		float64(usage.CachedTokens)/1_000_000*cacheReadRate +
		float64(usage.CompletionTokens)/1_000_000*r.OutputPerMillion
}

// CalcOpenAI implements OpenAI's billing model. OpenAI auto-caches eligible
// prompt prefixes (>=1024 tokens) and bills the cached portion at half the
// input rate by default. PromptTokens already includes the cached share, so
// we split out CachedTokens and bill the remainder at the plain input rate.
func CalcOpenAI(usage events.Usage, r Rates) float64 {
	cachedRate := r.CacheReadPerMillion
	if cachedRate == 0 {
		cachedRate = r.InputPerMillion * 0.5
	}
	plainInput := max(usage.PromptTokens-usage.CachedTokens, 0)
	return float64(plainInput)/1_000_000*r.InputPerMillion +
		float64(usage.CachedTokens)/1_000_000*cachedRate +
		float64(usage.CompletionTokens)/1_000_000*r.OutputPerMillion
}

// CalcGemini implements Gemini's billing model. CachedTokens are billed at
// CachedRatio × InputPerMillion (Google bills cached input ~25% of standard).
// PromptTokens includes the cached share, so the uncached remainder bills at
// the plain input rate. ReasoningTokens (thoughtTokenCount) bill at the
// output rate per Google's policy.
func CalcGemini(usage events.Usage, r Rates) float64 {
	cachedRatio := r.CachedRatio
	if cachedRatio == 0 {
		cachedRatio = 0.25
	}
	cachedCost := float64(usage.CachedTokens) / 1_000_000 * r.InputPerMillion * cachedRatio
	uncachedPrompt := max(usage.PromptTokens-usage.CachedTokens, 0)
	promptCost := float64(uncachedPrompt) / 1_000_000 * r.InputPerMillion
	outputCost := float64(usage.CompletionTokens+usage.ReasoningTokens) / 1_000_000 * r.OutputPerMillion
	return cachedCost + promptCost + outputCost
}

func cloneRates(in map[string]Rates) map[string]Rates {
	out := make(map[string]Rates, len(in))
	maps.Copy(out, in)
	return out
}

// numericFloat reads a key that could be a float64 or int from a YAML map.
func numericFloat(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
