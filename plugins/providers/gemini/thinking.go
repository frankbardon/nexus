package gemini

// thinkingConfig controls Gemini 2.5's reasoning ("thinking") behavior.
//
//	thinking:
//	  enabled: true
//	  budget_tokens: 8192       # max thought tokens; -1 = dynamic, 0 = disabled
//	  include_thoughts: true    # surface thought parts as thinking.step events
type thinkingConfig struct {
	Enabled         bool
	BudgetTokens    int  // -1 dynamic, 0 disabled (sent only when non-zero)
	IncludeThoughts bool // mirrors thinkingConfig.includeThoughts
}

func parseThinkingConfig(cfg map[string]any) thinkingConfig {
	tc := thinkingConfig{}

	raw, ok := cfg["thinking"].(map[string]any)
	if !ok {
		return tc
	}

	if v, ok := raw["enabled"].(bool); ok {
		tc.Enabled = v
	}
	if v, ok := raw["budget_tokens"].(int); ok {
		tc.BudgetTokens = v
	}
	if v, ok := raw["include_thoughts"].(bool); ok {
		tc.IncludeThoughts = v
	}

	return tc
}
