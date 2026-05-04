package gemini

import "github.com/frankbardon/nexus/pkg/engine/pricing"

// parsePricingConfig builds the merged price table from embedded defaults
// plus optional `pricing:` overrides under the plugin's config block.
//
//	pricing:
//	  gemini-2.5-pro:
//	    input_per_million: 2.50    # >200k tier
//	    output_per_million: 15.0
//	    cached_ratio: 0.25
func parsePricingConfig(cfg map[string]any) *pricing.Table {
	tbl := pricing.DefaultsFor(pricing.ProviderGemini)
	if raw, ok := cfg["pricing"].(map[string]any); ok {
		tbl.Merge(raw)
	}
	return tbl
}
