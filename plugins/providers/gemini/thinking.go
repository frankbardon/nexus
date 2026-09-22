package gemini

import (
	"fmt"
	"log/slog"
)

// thinkingMode is the operator-declared axis that decides which thinking
// parameter goes on the wire.
//
// Gemini 3.x requires generationConfig.thinkingConfig.thinkingLevel and does
// not support thinkingBudget; Gemini 2.5 is the inverse — thinkingBudget is the
// only correct parameter there and a level degrades 2.5 Pro. Sending both is a
// 400, so the two are mutually exclusive in config. The provider never inspects
// the model id to pick a shape: a mode that mismatches the target model is a
// provider error by design, and the operator declares which family they mean.
type thinkingMode string

const (
	// thinkingModeLevel emits thinkingConfig.thinkingLevel (Gemini 3.x).
	thinkingModeLevel thinkingMode = "level"
	// thinkingModeBudget emits thinkingConfig.thinkingBudget (Gemini 2.5).
	thinkingModeBudget thinkingMode = "budget"
	// thinkingModeOff emits no thinkingConfig at all.
	thinkingModeOff thinkingMode = "off"
)

// validThinkingModes is the closed set of values for thinking.mode.
var validThinkingModes = map[string]thinkingMode{
	string(thinkingModeLevel):  thinkingModeLevel,
	string(thinkingModeBudget): thinkingModeBudget,
	string(thinkingModeOff):    thinkingModeOff,
}

// validThinkingLevels is the closed set of values Gemini accepts for
// thinkingLevel. Four values, lowercase.
var validThinkingLevels = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
}

// thinkingConfig controls Gemini's reasoning ("thinking") behavior.
//
//	thinking:
//	  mode: level               # level | budget | off
//	  level: medium             # minimal | low | medium | high  (mode: level)
//	  budget_tokens: 8192       # -1 dynamic, 0 disable          (mode: budget)
//	  include_thoughts: true    # surface thought parts as thinking.step events
type thinkingConfig struct {
	Mode            thinkingMode
	Level           string // minimal | low | medium | high; set only under mode: level
	BudgetTokens    int    // -1 dynamic, 0 disable; meaningful only under mode: budget
	IncludeThoughts bool   // mirrors thinkingConfig.includeThoughts
}

// parseThinkingConfig reads the plugin's `thinking:` block. Every failure it can
// detect is a config error returned to Init, not a request-time surprise.
//
// Resolution, in order:
//   - no `thinking:` block at all          → off
//   - `mode:` set                          → that mode (`enabled` is ignored, warned)
//   - `enabled: false`                     → off (deprecated, warned)
//   - `budget_tokens` present              → budget (inferred, warned)
//   - `enabled: true`                      → level (deprecated, warned)
//   - a block with none of the above       → level
func parseThinkingConfig(cfg map[string]any, logger *slog.Logger) (thinkingConfig, error) {
	tc := thinkingConfig{Mode: thinkingModeOff}
	if logger == nil {
		logger = slog.Default()
	}

	raw, ok := cfg["thinking"].(map[string]any)
	if !ok {
		return tc, nil
	}

	// include_thoughts is independent of the mode axis — it is Gemini's whole
	// thought-visibility control and has no level/budget analogue.
	if v, ok := raw["include_thoughts"].(bool); ok {
		tc.IncludeThoughts = v
	}

	levelRaw, levelPresent := raw["level"]
	budgetRaw, budgetPresent := raw["budget_tokens"]
	if levelPresent && budgetPresent {
		return tc, fmt.Errorf("gemini: thinking.level and thinking.budget_tokens are mutually exclusive (Gemini rejects a request carrying both thinkingLevel and thinkingBudget) — keep one and set thinking.mode to match")
	}

	if levelPresent {
		s, ok := levelRaw.(string)
		if !ok || !validThinkingLevels[s] {
			return tc, fmt.Errorf("gemini: thinking.level must be one of minimal, low, medium, high (got %v)", levelRaw)
		}
		tc.Level = s
	}
	if budgetPresent {
		n, ok := thinkingBudgetValue(budgetRaw)
		if !ok {
			return tc, fmt.Errorf("gemini: thinking.budget_tokens must be an integer (got %v)", budgetRaw)
		}
		tc.BudgetTokens = n
	}

	modeRaw, modePresent := raw["mode"]
	enabled, enabledPresent := raw["enabled"].(bool)

	switch {
	case modePresent:
		s, ok := modeRaw.(string)
		if !ok {
			return tc, fmt.Errorf("gemini: thinking.mode must be one of level, budget, off (got %v)", modeRaw)
		}
		m, ok := validThinkingModes[s]
		if !ok {
			return tc, fmt.Errorf("gemini: thinking.mode must be one of level, budget, off (got %q)", s)
		}
		tc.Mode = m
		if enabledPresent {
			logger.Warn("thinking.enabled is deprecated and ignored when thinking.mode is set; remove it",
				"mode", string(tc.Mode))
		}
	case enabledPresent && !enabled:
		logger.Warn("thinking.enabled is deprecated; use thinking.mode instead", "resolved_mode", string(thinkingModeOff))
		tc.Mode = thinkingModeOff
	case budgetPresent:
		if enabledPresent {
			logger.Warn("thinking.enabled is deprecated; use thinking.mode instead", "resolved_mode", string(thinkingModeBudget))
		}
		logger.Warn("thinking.budget_tokens is set without thinking.mode; inferring thinking.mode: budget (correct for Gemini 2.5, rejected by Gemini 3.x) — set thinking.mode explicitly")
		tc.Mode = thinkingModeBudget
	case enabledPresent:
		logger.Warn("thinking.enabled is deprecated; use thinking.mode instead", "resolved_mode", string(thinkingModeLevel))
		tc.Mode = thinkingModeLevel
	default:
		tc.Mode = thinkingModeLevel
	}

	switch tc.Mode {
	case thinkingModeLevel:
		if tc.Level == "" {
			return tc, fmt.Errorf("gemini: thinking.mode is %q but thinking.level is not set (one of minimal, low, medium, high)", thinkingModeLevel)
		}
	case thinkingModeBudget:
		if !budgetPresent {
			return tc, fmt.Errorf("gemini: thinking.mode is %q but thinking.budget_tokens is not set (-1 dynamic, 0 disable, or a positive token budget)", thinkingModeBudget)
		}
	}

	return tc, nil
}

// thinkingBudgetValue accepts both decode shapes: YAML lands an integer as an
// int or a float64 depending on the path it took.
func thinkingBudgetValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// applyThinking writes the thinkingConfig object into the generationConfig map.
// Exactly one of thinkingLevel / thinkingBudget ever goes out, and mode: off
// writes nothing at all.
func applyThinking(gen map[string]any, tc thinkingConfig) {
	cfg := map[string]any{}

	switch tc.Mode {
	case thinkingModeLevel:
		cfg["thinkingLevel"] = tc.Level
	case thinkingModeBudget:
		// -1 (dynamic) and 0 (disable) are both meaningful on Gemini 2.5, so
		// the budget goes out whatever its value.
		cfg["thinkingBudget"] = tc.BudgetTokens
	default:
		return
	}

	if tc.IncludeThoughts {
		cfg["includeThoughts"] = true
	}
	gen["thinkingConfig"] = cfg
}
