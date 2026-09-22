package gemini

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
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

	// LevelSource records which configuration layer named Level, which is what
	// decides whether it outranks a role's `effort:`. The zero value is
	// levelFromPlugin, so every plain parseThinkingConfig result — including
	// the plugin-level one Init stores — is correctly marked as the least
	// specific layer.
	LevelSource levelSource
}

// levelSource names the layer a thinkingConfig.Level came from. Only two of the
// four precedence layers can supply one, and the pair is exactly the question
// thinkingLevelFor asks.
type levelSource int

const (
	// levelFromPlugin: the plugin-level `thinking.level`. The least specific
	// statement of depth there is, so a role's `effort:` outranks it.
	levelFromPlugin levelSource = iota
	// levelFromRole: the `thinking:` block on the request — the `core.models`
	// entry the request resolved to, or the one a coordinator stamped for the
	// chain entry actually being served. More specific than the same role's
	// `effort:`, and written in Gemini's own vocabulary besides.
	levelFromRole
)

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
// Precedence is the one rule the whole codebase now follows — specificity wins:
//
//  1. a `level` named by the request's own `thinking:` block, which is either
//     the serving chain entry's block stamped by a coordinator or the named
//     role's block recovered from the registry;
//  2. the role's `effort:`, translated into Gemini's vocabulary;
//  3. the plugin-level `thinking.level`.
//
// Until this story steps 2 and 3 were the other way round on this provider and
// only on this provider, on the argument that `level` is Gemini's native
// vocabulary and `effort` the translated one. That argument does not survive a
// role being able to write `level` itself: a plugin-level key is a default, and
// a per-role key — in either vocabulary — is the more specific statement.
//
// An empty result means "send no thinkingLevel" and let the model's own default
// level stand.
func thinkingLevelFor(tc thinkingConfig, effort string) (string, error) {
	// A level the role named outranks its own effort, so an unrecognised
	// effort cannot fail a request whose depth was never going to read it.
	if tc.LevelSource == levelFromRole && tc.Level != "" {
		return tc.Level, nil
	}
	level, _, ok := resolveEffortLevel(effort)
	if !ok {
		return "", errUnknownEffort(effort)
	}
	if level != "" {
		return level, nil
	}
	return tc.Level, nil
}

// validateRoleThinking sweeps every core.models role this provider could serve,
// at Init, and checks both halves of what a role entry can say about reasoning
// depth: its native `thinking:` block and its `effort:`.
//
// The two are checked together because they cannot be checked apart. Whether a
// role's `effort` can reach the wire at all depends on the mode and the level
// of the block that role resolves to, and with per-role blocks that is no longer
// the plugin-level configuration — it is the role's block merged over it.
//
// Blocks: a block on a core.models entry bypasses this plugin's schema.json
// entirely — core stores these maps without looking inside them — so nothing
// else ever checks them. Without this sweep a role whose merged block is invalid
// (`mode: budget` with no budget, or a `level` and a `budget_tokens` the role
// named together) would boot clean and fail at the first request that named it.
// The error names the role, which is the useful half of the answer.
//
// Efforts: the clamp of Anthropic's xhigh/max onto Gemini's high is warned here
// and nowhere else — it is a configuration fact, and a per-request warning would
// flood the logs on a busy fanout role. It is warned once per (role, value).
// Validation runs only where the effort can actually reach the wire: under
// mode: level, and only when the role's own block did not name a `level` to
// outrank it. Under mode: budget and mode: off role effort has no meaning on
// this provider and is ignored silently — as it is, now, behind a role's own
// level. A plugin-level `thinking.level` no longer shadows it: since this story
// the role's effort outranks that key, so the value does reach the wire and is
// checked.
//
// Entries naming another provider are that provider's business and are skipped;
// an entry naming none may land here, so it is checked. The whole chain is
// walked, not just the primary: a fallback entry's block and effort both reach
// this provider through the coordinator's stamp and are just as capable of being
// wrong.
//
// pluginTC is the already-parsed plugin-level configuration, used verbatim for
// every entry that carries no block of its own. Re-parsing the plugin block per
// role would repeat its deprecation warnings once per role; Init has already
// said them once.
func validateRoleThinking(models *engine.ModelRegistry, plugin map[string]any, pluginTC thinkingConfig, logger *slog.Logger) error {
	if models == nil {
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
			if cfg.Provider != "" && cfg.Provider != pluginID {
				continue
			}

			tc := pluginTC
			if cfg.Thinking != nil {
				// The role's logger, so a deprecation warning raised by the
				// merged block says which role raised it.
				merged, err := parseMergedThinking(plugin, cfg.Thinking, logger.With("role", role))
				if err != nil {
					return fmt.Errorf("core.models role %q: %w", role, err)
				}
				tc = merged
			}

			if cfg.Effort == "" {
				continue
			}
			// Where the effort cannot reach the wire, its vocabulary is not
			// this provider's business.
			if tc.Mode != thinkingModeLevel || (tc.LevelSource == levelFromRole && tc.Level != "") {
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
// It is read only under mode: level, and only where the resolved block did not
// name a `level` of its own — see thinkingLevelFor. An unrecognised effort is
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

// --- per-role thinking ------------------------------------------------------

// rawThinkingBlock lifts the plugin-level `thinking:` block off the plugin
// config so it can be merged with a core.models role's block later. nil means
// the plugin set no block at all, which parseThinkingConfig reads as mode off.
//
// The map is not copied: everything reachable from it is treated as read-only,
// and mergeThinkingBlock always builds a fresh map rather than writing into
// either input.
func rawThinkingBlock(cfg map[string]any) map[string]any {
	block, ok := cfg["thinking"].(map[string]any)
	if !ok {
		return nil
	}
	return block
}

// mergeThinkingBlock merges a core.models role's `thinking:` block over the
// plugin-level one, key by key. The role wins on every key it mentions, and a
// plugin-level key the role is silent about survives — so a role that only wants
// a different `include_thoughts` does not have to restate the mode.
//
// A nil role block means the role said nothing, and the plugin block stands
// unchanged. A set-but-empty role block (`thinking: {}`) is a statement rather
// than a gap: it overrides no individual key, but the merged block is non-nil,
// so a role can turn thinking on (parseThinkingConfig reads a present block with
// no mode as mode: level) on a deployment whose plugin block is absent entirely.
//
// One Gemini-specific departure from a plain key-wise merge, and it exists to
// keep this provider's central invariant true: `level` and `budget_tokens`
// produce thinkingLevel and thinkingBudget, Gemini rejects a request carrying
// both, and so the two keys are mutually exclusive in config. A role that names
// one therefore *displaces* an inherited other rather than landing beside it.
// Without that, the commonest per-role configuration there is — a Gemini 2.5
// plugin block and a Gemini 3.x role, or the reverse — would merge into a block
// no operator could have written and fail the boot. A role that names both keys
// itself is still an error: that one the operator did write.
//
// The remaining sharp edge, accepted deliberately: a role that switches `mode`
// without restating the key that mode needs inherits a block that does not
// satisfy it — a plugin-level `mode: budget` plus a role's bare `level:` leaves
// `mode: budget` with the budget displaced. That is caught at Init by
// validateRoleThinking, naming the role. Restating `mode` on the role is the way
// out.
//
// Neither input is mutated and the result aliases neither: the plugin config map
// and the registry's block are both shared with the loaded configuration.
func mergeThinkingBlock(plugin, role map[string]any) map[string]any {
	if role == nil {
		return plugin
	}
	merged := make(map[string]any, len(plugin)+len(role))
	for k, v := range plugin {
		merged[k] = v
	}
	_, roleLevel := role["level"]
	_, roleBudget := role["budget_tokens"]
	if roleLevel && !roleBudget {
		delete(merged, "budget_tokens")
	}
	if roleBudget && !roleLevel {
		delete(merged, "level")
	}
	for k, v := range role {
		merged[k] = v
	}
	return merged
}

// parseMergedThinking runs a merged block back through parseThinkingConfig —
// the one parser — by handing it the block wrapped in the shape it expects. A
// merged block is an ordinary `thinking:` block and gets exactly the same
// validation, inference and deprecation handling the plugin-level one does.
//
// The one thing the parser cannot see is *which* layer named `level`, so that is
// recorded here: only a level the role itself named outranks the role's effort.
//
// A nil logger suppresses the deprecation warnings, which is what the
// per-request path wants: validateRoleThinking has already run every role's
// merged block through here once at Init with the real logger attached.
func parseMergedThinking(plugin, role map[string]any, logger *slog.Logger) (thinkingConfig, error) {
	merged := mergeThinkingBlock(plugin, role)
	if merged == nil {
		return thinkingConfig{Mode: thinkingModeOff}, nil
	}
	tc, err := parseThinkingConfig(map[string]any{"thinking": merged}, logger)
	if err != nil {
		return tc, err
	}
	if _, named := role["level"]; named && tc.Level != "" {
		tc.LevelSource = levelFromRole
	}
	return tc, nil
}

// resolveThinking picks the thinking configuration for one request.
//
// This is the single lookup point for the thinking block, deliberately separate
// from its use site. Precedence, most specific first:
//
//  1. req.Overrides.Thinking — the block of the chain entry actually being
//     served. It arrives either stamped by the fallback or fanout coordinator
//     (which alone knows a non-first entry is in play) or recovered from the
//     registry by engine.ResolveModelConfig on the paths no coordinator
//     touches. Either way it is merged over, not substituted for, the plugin
//     block.
//  2. the role's `effort:`, which becomes a thinkingLevel only where the
//     resolved block leaves room for it — see thinkingLevelFor.
//  3. the plugin-level `thinking:` block, which is what a request carrying no
//     override of its own gets, unchanged.
//
// An unparseable merged block is unreachable through Init, which sweeps every
// role this provider could serve and refuses to boot on one. The degradation
// here exists for a request whose Overrides.Thinking was hand-set by something
// other than the registry: fall back to the plugin-level configuration rather
// than putting a shape Gemini will 400 on onto the wire.
func (p *Plugin) resolveThinking(req events.LLMRequest) thinkingConfig {
	if req.Overrides.Thinking == nil {
		return p.thinking
	}
	// Nil logger: the warnings are boot-time facts, already said once by
	// validateRoleThinking. Repeating them per request would flood the log of
	// a busy role.
	tc, err := parseMergedThinking(p.thinkingRaw, req.Overrides.Thinking, nil)
	if err != nil {
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("gemini: ignoring an invalid per-request thinking block; using the plugin-level configuration",
			"role", req.Role,
			"error", err,
		)
		return p.thinking
	}
	return tc
}
