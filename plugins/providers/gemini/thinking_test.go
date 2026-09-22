package gemini

import (
	"io"
	"log/slog"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustParseThinking(t *testing.T, cfg map[string]any) thinkingConfig {
	t.Helper()
	tc, err := parseThinkingConfig(cfg, quietLogger())
	if err != nil {
		t.Fatalf("parseThinkingConfig: %v", err)
	}
	return tc
}

func TestParseThinkingConfig_AbsentBlockIsOff(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{})
	if tc.Mode != thinkingModeOff {
		t.Fatalf("expected mode off, got %+v", tc)
	}
}

func TestParseThinkingConfig_PresentBlockDefaultsToLevel(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"level": "medium"},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "medium" {
		t.Fatalf("expected mode level / medium, got %+v", tc)
	}
}

func TestParseThinkingConfig_ExplicitLevelMode(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"mode":             "level",
			"level":            "high",
			"include_thoughts": true,
		},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "high" || !tc.IncludeThoughts {
		t.Fatalf("unexpected: %+v", tc)
	}
}

func TestParseThinkingConfig_AllFourLevels(t *testing.T) {
	for _, lvl := range []string{"minimal", "low", "medium", "high"} {
		tc := mustParseThinking(t, map[string]any{
			"thinking": map[string]any{"mode": "level", "level": lvl},
		})
		if tc.Level != lvl {
			t.Fatalf("level %q did not round-trip: %+v", lvl, tc)
		}
	}
}

func TestParseThinkingConfig_RejectsUnknownLevel(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "level", "level": "xhigh"},
	}, quietLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown level")
	}
}

func TestParseThinkingConfig_RejectsUnknownMode(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "adaptive", "level": "high"},
	}, quietLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}

func TestParseThinkingConfig_LevelModeNeedsLevel(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "level"},
	}, quietLogger())
	if err == nil {
		t.Fatal("expected mode: level without a level to fail")
	}
}

func TestParseThinkingConfig_BudgetModeNeedsBudget(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"mode": "budget"},
	}, quietLogger())
	if err == nil {
		t.Fatal("expected mode: budget without budget_tokens to fail")
	}
}

func TestParseThinkingConfig_LevelAndBudgetAreMutuallyExclusive(t *testing.T) {
	_, err := parseThinkingConfig(map[string]any{
		"thinking": map[string]any{"level": "high", "budget_tokens": 8192},
	}, quietLogger())
	if err == nil {
		t.Fatal("expected level + budget_tokens together to fail")
	}
}

func TestParseThinkingConfig_BudgetModeDynamicAndDisable(t *testing.T) {
	for _, want := range []int{-1, 0, 8192} {
		tc := mustParseThinking(t, map[string]any{
			"thinking": map[string]any{"mode": "budget", "budget_tokens": want},
		})
		if tc.Mode != thinkingModeBudget || tc.BudgetTokens != want {
			t.Fatalf("budget %d did not round-trip: %+v", want, tc)
		}
	}
}

func TestParseThinkingConfig_FloatBudget(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "budget", "budget_tokens": float64(4096)},
	})
	if tc.BudgetTokens != 4096 {
		t.Fatalf("expected 4096, got %+v", tc)
	}
}

func TestParseThinkingConfig_BudgetWithoutModeInfersBudget(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"budget_tokens": 4096},
	})
	if tc.Mode != thinkingModeBudget || tc.BudgetTokens != 4096 {
		t.Fatalf("expected inferred budget mode, got %+v", tc)
	}
}

func TestParseThinkingConfig_EnabledTrueIsLevel(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": true, "level": "low"},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "low" {
		t.Fatalf("expected deprecated enabled:true to resolve to level, got %+v", tc)
	}
}

func TestParseThinkingConfig_EnabledFalseIsOff(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": false, "budget_tokens": 4096},
	})
	if tc.Mode != thinkingModeOff {
		t.Fatalf("expected deprecated enabled:false to resolve to off, got %+v", tc)
	}
	if tc.BudgetTokens != 4096 {
		t.Fatalf("budget should still parse, got %d", tc.BudgetTokens)
	}
}

// The one legacy configuration that works today (Gemini 2.5 + a budget) must
// keep emitting a budget, not a level.
func TestParseThinkingConfig_LegacyEnabledPlusBudgetStaysBudget(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{
			"enabled":          true,
			"budget_tokens":    4096,
			"include_thoughts": true,
		},
	})
	if tc.Mode != thinkingModeBudget || tc.BudgetTokens != 4096 || !tc.IncludeThoughts {
		t.Fatalf("unexpected: %+v", tc)
	}
}

func TestParseThinkingConfig_ModeWinsOverEnabled(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "off", "enabled": true},
	})
	if tc.Mode != thinkingModeOff {
		t.Fatalf("expected mode to win over enabled, got %+v", tc)
	}
}

func TestApplyThinking_LevelModeNeverSendsBudget(t *testing.T) {
	gen := map[string]any{}
	applyThinking(gen, thinkingConfig{Mode: thinkingModeLevel, Level: "high", BudgetTokens: 8192, IncludeThoughts: true})

	tc, ok := gen["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected a thinkingConfig, got %#v", gen)
	}
	if tc["thinkingLevel"] != "high" {
		t.Fatalf("expected thinkingLevel high, got %#v", tc)
	}
	if _, present := tc["thinkingBudget"]; present {
		t.Fatalf("thinkingBudget must never ride the level path: %#v", tc)
	}
	if tc["includeThoughts"] != true {
		t.Fatalf("expected includeThoughts, got %#v", tc)
	}
}

func TestApplyThinking_BudgetModeNeverSendsLevel(t *testing.T) {
	gen := map[string]any{}
	applyThinking(gen, thinkingConfig{Mode: thinkingModeBudget, Level: "high", BudgetTokens: 4096})

	tc, ok := gen["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected a thinkingConfig, got %#v", gen)
	}
	if tc["thinkingBudget"] != 4096 {
		t.Fatalf("expected thinkingBudget 4096, got %#v", tc)
	}
	if _, present := tc["thinkingLevel"]; present {
		t.Fatalf("thinkingLevel must never ride the budget path: %#v", tc)
	}
	if _, present := tc["includeThoughts"]; present {
		t.Fatalf("includeThoughts should be absent when not enabled: %#v", tc)
	}
}

func TestApplyThinking_BudgetZeroAndDynamicSurvive(t *testing.T) {
	for _, want := range []int{-1, 0} {
		gen := map[string]any{}
		applyThinking(gen, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: want})
		tc, ok := gen["thinkingConfig"].(map[string]any)
		if !ok {
			t.Fatalf("expected a thinkingConfig for budget %d, got %#v", want, gen)
		}
		if tc["thinkingBudget"] != want {
			t.Fatalf("expected thinkingBudget %d, got %#v", want, tc)
		}
	}
}

func TestApplyThinking_OffWritesNothing(t *testing.T) {
	gen := map[string]any{}
	applyThinking(gen, thinkingConfig{Mode: thinkingModeOff, Level: "high", BudgetTokens: 4096, IncludeThoughts: true})
	if len(gen) != 0 {
		t.Fatalf("mode off must write no thinkingConfig, got %#v", gen)
	}
}
