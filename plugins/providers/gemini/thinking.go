package gemini

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/frankbardon/nexus/pkg/engine"
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

	// mode: budget has no meaningful default — a budget is a number only the
	// operator can pick — so a missing budget_tokens is an error. mode: level
	// is the opposite: every Gemini 3.x model carries its own default thinking
	// level (3.1 Pro high, 3.x Flash medium, Flash-Lite minimal), so omitting
	// thinking.level is a valid way to say "use the model default" and must not
	// fail the boot. It is also what keeps a bare `thinking: {enabled: true}`
	// working here the way it does on Anthropic.
	if tc.Mode == thinkingModeBudget && !budgetPresent {
		return tc, fmt.Errorf("gemini: thinking.mode is %q but thinking.budget_tokens is not set (-1 dynamic, 0 disable, or a positive token budget)", thinkingModeBudget)
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

// effortThinkingLevels translates the shared, provider-interpreted
// core.models `effort:` vocabulary into Gemini's four-value thinkingLevel
// vocabulary.
//
// The two vocabularies do not line up — Anthropic reads low|medium|high|xhigh|max
// and Gemini minimal|low|medium|high — and a `core.models` fanout role can
// dispatch one role to both providers at once. Gemini therefore clamps what it
// cannot express rather than ignoring the value (which would make `effort:` a
// silent no-op on this leg) or rejecting it (which would make any fanout role
// using Anthropic's top two levels unconfigurable).
var effortThinkingLevels = map[string]string{
	// Gemini's own four, verbatim.
	"minimal": "minimal",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	// Anthropic's top two have no Gemini analogue: "as deep as this model
	// goes" is `high` here.
	"xhigh": "high",
	"max":   "high",
}

// resolveEffortLevel maps a core.models effort onto a Gemini thinkingLevel.
// An empty effort resolves to an empty level ("not set"). ok is false for a
// value in neither provider's vocabulary — a typo, not a depth.
func resolveEffortLevel(effort string) (level string, clamped, ok bool) {
	if effort == "" {
		return "", false, true
	}
	level, found := effortThinkingLevels[effort]
	if !found {
		return "", false, false
	}
	return level, level != effort, true
}

// errUnknownEffort builds the one message both the Init-time and the
// request-time rejection use.
func errUnknownEffort(effort string) error {
	return fmt.Errorf("gemini: effort %q is not a recognised reasoning-depth value (Gemini: minimal, low, medium, high; Anthropic: low, medium, high, xhigh, max)", effort)
}

// thinkingLevelFor picks the thinkingLevel for one request under mode: level.
//
// Precedence is deliberately the inverse of the Anthropic provider's: the
// plugin-level `thinking.level` wins over the role's `effort`, because `level`
// is Gemini's own native vocabulary and role effort is the translated,
// cross-provider one. An empty result means "send no thinkingLevel" and let the
// model's default level stand.
func thinkingLevelFor(tc thinkingConfig, effort string) (string, error) {
	if tc.Level != "" {
		return tc.Level, nil
	}
	level, _, ok := resolveEffortLevel(effort)
	if !ok {
		return "", errUnknownEffort(effort)
	}
	return level, nil
}

// validateRoleEfforts checks every core.models role this provider could serve
// against Gemini's thinkingLevel vocabulary, at Init.
//
// The clamp is warned here and nowhere else: it is a configuration fact, and a
// per-request warning would flood the logs on a busy fanout role. Validation
// runs only where the effort can actually reach the wire — under mode: level
// with no plugin-level thinking.level to override it. Under mode: budget and
// mode: off, and whenever thinking.level is set, role effort has no meaning on
// this provider and is ignored silently.
func validateRoleEfforts(models *engine.ModelRegistry, tc thinkingConfig, logger *slog.Logger) error {
	if models == nil || tc.Mode != thinkingModeLevel || tc.Level != "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	roles := models.Roles()
	sort.Strings(roles)
	warned := map[string]bool{}

	for _, role := range roles {
		for i := 0; i < models.ChainLen(role); i++ {
			cfg, ok := models.Fallback(role, i)
			if !ok {
				continue
			}
			// An entry naming another provider is that provider's business.
			// An entry naming none may land here, so it is checked.
			if cfg.Provider != "" && cfg.Provider != pluginID {
				continue
			}
			if cfg.Effort == "" {
				continue
			}
			level, clamped, ok := resolveEffortLevel(cfg.Effort)
			if !ok {
				return fmt.Errorf("core.models role %q: %w", role, errUnknownEffort(cfg.Effort))
			}
			if !clamped {
				continue
			}
			key := role + "\x00" + cfg.Effort
			if warned[key] {
				continue
			}
			warned[key] = true
			logger.Warn("core.models effort clamped to Gemini's thinkingLevel range",
				"role", role,
				"configured", cfg.Effort,
				"effective", level,
			)
		}
	}
	return nil
}

// applyThinking writes the thinkingConfig object into the generationConfig map.
// Exactly one of thinkingLevel / thinkingBudget ever goes out, and mode: off
// writes nothing at all.
//
// effort is the request's core.models role effort (events.LLMRequest.Effort).
// It is read only under mode: level, and only as a fallback behind the
// plugin-level thinking.level — see thinkingLevelFor. An unrecognised effort is
// an error rather than a silent clamp, which fails the request through
// buildRequestBody instead of quietly sending a depth nobody asked for.
func applyThinking(gen map[string]any, tc thinkingConfig, effort string) error {
	cfg := map[string]any{}

	switch tc.Mode {
	case thinkingModeLevel:
		level, err := thinkingLevelFor(tc, effort)
		if err != nil {
			return err
		}
		// An empty level means "omit thinkingLevel": the model's own default
		// level applies. thinkingConfig still goes out, because
		// includeThoughts may need to ride it.
		if level != "" {
			cfg["thinkingLevel"] = level
		}
	case thinkingModeBudget:
		// -1 (dynamic) and 0 (disable) are both meaningful on Gemini 2.5, so
		// the budget goes out whatever its value.
		cfg["thinkingBudget"] = tc.BudgetTokens
	default:
		return nil
	}

	if tc.IncludeThoughts {
		cfg["includeThoughts"] = true
	}
	gen["thinkingConfig"] = cfg
	return nil
}
