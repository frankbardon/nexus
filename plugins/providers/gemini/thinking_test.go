package gemini

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
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

func mustApplyThinking(t *testing.T, gen map[string]any, tc thinkingConfig, effort string) {
	t.Helper()
	if err := applyThinking(gen, tc, effort); err != nil {
		t.Fatalf("applyThinking: %v", err)
	}
}

// thinkingConfigOf runs applyThinking and returns the emitted thinkingConfig
// object, failing the test when none was written.
func thinkingConfigOf(t *testing.T, tc thinkingConfig, effort string) map[string]any {
	t.Helper()
	gen := map[string]any{}
	mustApplyThinking(t, gen, tc, effort)
	out, ok := gen["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected a thinkingConfig, got %#v", gen)
	}
	return out
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

// Every Gemini 3.x model carries its own default thinking level, so omitting
// thinking.level under mode: level means "use the model default" — a valid
// configuration, not a boot failure. It is also what keeps a bare
// `thinking: {enabled: true}` working here the way it does on Anthropic.
func TestParseThinkingConfig_LevelModeWithoutLevelIsModelDefault(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"mode": "level"},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "" {
		t.Fatalf("expected mode level with no level, got %+v", tc)
	}
}

func TestParseThinkingConfig_BareEnabledTrueIsModelDefaultLevel(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"enabled": true},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "" {
		t.Fatalf("expected a bare enabled:true to resolve to mode level, got %+v", tc)
	}
}

func TestParseThinkingConfig_BareIncludeThoughtsIsModelDefaultLevel(t *testing.T) {
	tc := mustParseThinking(t, map[string]any{
		"thinking": map[string]any{"include_thoughts": true},
	})
	if tc.Mode != thinkingModeLevel || tc.Level != "" || !tc.IncludeThoughts {
		t.Fatalf("expected a bare include_thoughts block to resolve to mode level, got %+v", tc)
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
	mustApplyThinking(t, gen, thinkingConfig{Mode: thinkingModeLevel, Level: "high", BudgetTokens: 8192, IncludeThoughts: true}, "")

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
	mustApplyThinking(t, gen, thinkingConfig{Mode: thinkingModeBudget, Level: "high", BudgetTokens: 4096}, "")

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
		mustApplyThinking(t, gen, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: want}, "")
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
	mustApplyThinking(t, gen, thinkingConfig{Mode: thinkingModeOff, Level: "high", BudgetTokens: 4096, IncludeThoughts: true}, "")
	if len(gen) != 0 {
		t.Fatalf("mode off must write no thinkingConfig, got %#v", gen)
	}
}

// --- role effort (core.models `effort:`) ---------------------------------

func TestApplyThinking_LevelModeWithoutLevelOmitsThinkingLevel(t *testing.T) {
	cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeLevel, IncludeThoughts: true}, "")
	if _, present := cfg["thinkingLevel"]; present {
		t.Fatalf("an unset level must leave the model default in place: %#v", cfg)
	}
	if cfg["includeThoughts"] != true {
		t.Fatalf("includeThoughts must still ride the block: %#v", cfg)
	}
}

func TestApplyThinking_EffortPassesThroughNativeLevels(t *testing.T) {
	for _, want := range []string{"minimal", "low", "medium", "high"} {
		cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeLevel}, want)
		if cfg["thinkingLevel"] != want {
			t.Fatalf("effort %q should pass through unchanged, got %#v", want, cfg)
		}
	}
}

func TestApplyThinking_EffortClampsAnthropicTopLevels(t *testing.T) {
	for _, effort := range []string{"xhigh", "max"} {
		cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeLevel}, effort)
		if cfg["thinkingLevel"] != "high" {
			t.Fatalf("effort %q should clamp to high, got %#v", effort, cfg)
		}
	}
}

func TestApplyThinking_UnknownEffortIsAnError(t *testing.T) {
	gen := map[string]any{}
	err := applyThinking(gen, thinkingConfig{Mode: thinkingModeLevel}, "ludicrous")
	if err == nil {
		t.Fatal("expected an unrecognised effort to fail the request")
	}
	if !strings.Contains(err.Error(), "ludicrous") {
		t.Fatalf("error should name the offending value, got %v", err)
	}
	if _, present := gen["thinkingConfig"]; present {
		t.Fatalf("a rejected effort must not leave a thinkingConfig behind: %#v", gen)
	}
}

// The plugin-level `level` is Gemini's native vocabulary; role effort is the
// translated one, so `level` wins. This is the inverse of the Anthropic
// precedence, on purpose.
func TestApplyThinking_PluginLevelWinsOverRoleEffort(t *testing.T) {
	cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeLevel, Level: "minimal"}, "max")
	if cfg["thinkingLevel"] != "minimal" {
		t.Fatalf("plugin-level level must win over role effort, got %#v", cfg)
	}
}

// An unrecognised effort cannot fail a request whose level never reads it.
func TestApplyThinking_PluginLevelShadowsAnUnknownEffort(t *testing.T) {
	cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeLevel, Level: "high"}, "ludicrous")
	if cfg["thinkingLevel"] != "high" {
		t.Fatalf("expected the plugin-level level, got %#v", cfg)
	}
}

func TestApplyThinking_EffortIgnoredUnderBudgetMode(t *testing.T) {
	cfg := thinkingConfigOf(t, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 4096}, "max")
	if _, present := cfg["thinkingLevel"]; present {
		t.Fatalf("role effort has no meaning under mode: budget: %#v", cfg)
	}
	if cfg["thinkingBudget"] != 4096 {
		t.Fatalf("expected the budget to survive, got %#v", cfg)
	}
}

func TestApplyThinking_EffortIgnoredUnderOffMode(t *testing.T) {
	gen := map[string]any{}
	mustApplyThinking(t, gen, thinkingConfig{Mode: thinkingModeOff}, "ludicrous")
	if len(gen) != 0 {
		t.Fatalf("mode off must write nothing whatever the effort, got %#v", gen)
	}
}

// --- Init-time validation of core.models efforts --------------------------

func registryWithEfforts(t *testing.T, raw map[string]any) *engine.ModelRegistry {
	t.Helper()
	return engine.NewModelRegistry(raw)
}

func TestValidateRoleEfforts_WarnsOnceOnClamp(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	models := registryWithEfforts(t, map[string]any{
		"deep": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{"provider": "nexus.llm.anthropic", "model": "claude", "effort": "max"},
				map[string]any{"provider": pluginID, "model": "gemini-3.1-pro", "effort": "xhigh"},
			},
		},
	})

	if err := validateRoleEfforts(models, thinkingConfig{Mode: thinkingModeLevel}, logger); err != nil {
		t.Fatalf("validateRoleEfforts: %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, "clamped"); n != 1 {
		t.Fatalf("expected exactly one clamp warning, got %d: %s", n, out)
	}
	for _, want := range []string{"role=deep", "configured=xhigh", "effective=high"} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning should contain %q, got %s", want, out)
		}
	}
}

func TestValidateRoleEfforts_NativeLevelsAreSilent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	models := registryWithEfforts(t, map[string]any{
		"fast": map[string]any{"provider": pluginID, "model": "gemini-3.5-flash", "effort": "minimal"},
	})

	if err := validateRoleEfforts(models, thinkingConfig{Mode: thinkingModeLevel}, logger); err != nil {
		t.Fatalf("validateRoleEfforts: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a native level must not warn, got %s", buf.String())
	}
}

func TestValidateRoleEfforts_UnknownValueFailsInit(t *testing.T) {
	models := registryWithEfforts(t, map[string]any{
		"balanced": map[string]any{"provider": pluginID, "model": "gemini-3.5-flash", "effort": "ludicrous"},
	})

	err := validateRoleEfforts(models, thinkingConfig{Mode: thinkingModeLevel}, quietLogger())
	if err == nil {
		t.Fatal("expected an unrecognised effort to fail Init")
	}
	if !strings.Contains(err.Error(), "balanced") || !strings.Contains(err.Error(), "ludicrous") {
		t.Fatalf("error should name the role and the value, got %v", err)
	}
}

// Another provider's vocabulary is not this provider's business.
func TestValidateRoleEfforts_IgnoresForeignProviderEntries(t *testing.T) {
	models := registryWithEfforts(t, map[string]any{
		"balanced": map[string]any{"provider": "nexus.llm.openai", "model": "gpt", "effort": "ludicrous"},
	})

	if err := validateRoleEfforts(models, thinkingConfig{Mode: thinkingModeLevel}, quietLogger()); err != nil {
		t.Fatalf("a foreign entry must not fail this provider's Init: %v", err)
	}
}

func TestValidateRoleEfforts_SilentUnderBudgetAndOff(t *testing.T) {
	models := registryWithEfforts(t, map[string]any{
		"balanced": map[string]any{"provider": pluginID, "model": "gemini-2.5-pro", "effort": "ludicrous"},
	})

	for _, mode := range []thinkingMode{thinkingModeBudget, thinkingModeOff} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		if err := validateRoleEfforts(models, thinkingConfig{Mode: mode, BudgetTokens: 4096}, logger); err != nil {
			t.Fatalf("mode %q must ignore role effort entirely: %v", mode, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("mode %q must not warn, got %s", mode, buf.String())
		}
	}
}

// A plugin-level level shadows role effort at request time, so it shadows the
// Init-time check too — nothing there can reach the wire.
func TestValidateRoleEfforts_SilentWhenPluginLevelSet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	models := registryWithEfforts(t, map[string]any{
		"balanced": map[string]any{"provider": pluginID, "model": "gemini-3.1-pro", "effort": "max"},
	})

	if err := validateRoleEfforts(models, thinkingConfig{Mode: thinkingModeLevel, Level: "low"}, logger); err != nil {
		t.Fatalf("validateRoleEfforts: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("an overridden effort must not warn, got %s", buf.String())
	}
}

func TestValidateRoleEfforts_NilRegistry(t *testing.T) {
	if err := validateRoleEfforts(nil, thinkingConfig{Mode: thinkingModeLevel}, quietLogger()); err != nil {
		t.Fatalf("a nil registry must be a no-op: %v", err)
	}
}
