package gemini

import "testing"

func TestParseThinkingConfig_Empty(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{})
	if tc.Enabled || tc.BudgetTokens != 0 || tc.IncludeThoughts {
		t.Fatalf("expected zero values, got %+v", tc)
	}
}

func TestParseThinkingConfig_Full(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled":          true,
			"budget_tokens":    8192,
			"include_thoughts": true,
		},
	})
	if !tc.Enabled || tc.BudgetTokens != 8192 || !tc.IncludeThoughts {
		t.Fatalf("unexpected: %+v", tc)
	}
}

func TestParseThinkingConfig_DisabledExplicit(t *testing.T) {
	tc := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{
			"enabled":       false,
			"budget_tokens": 4096,
		},
	})
	if tc.Enabled {
		t.Fatal("expected enabled=false")
	}
	if tc.BudgetTokens != 4096 {
		t.Fatalf("budget should still parse, got %d", tc.BudgetTokens)
	}
}
