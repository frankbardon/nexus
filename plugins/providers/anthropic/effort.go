package anthropic

import (
	"fmt"
	"log/slog"
)

// effortLevel is Anthropic's named reasoning-depth control — the replacement
// for `thinking.budget_tokens`, which is an HTTP 400 on every current model
// family. It rides the wire inside `output_config`, never as a top-level
// request field.
//
// It is deliberately independent of thinkingConfig.Mode: effort is emitted
// whatever the thinking mode is, including thinkingModeOff, because it governs
// how hard the model works on the answer rather than whether it emits thinking
// blocks.
//
// As everywhere else in this provider there is no model-capability table. Not
// every model accepts every level (Opus 4.6 / Sonnet 4.6 top out at `max` with
// no `xhigh`; Sonnet 4.5 and Haiku 4.5 reject effort outright), and a level the
// target model does not accept is an Anthropic 400 the operator owns.
type effortLevel string

const (
	// effortUnset sends no `effort` key at all, so the API default (`high`)
	// applies. This is the zero value, and the default.
	effortUnset effortLevel = ""

	effortLow    effortLevel = "low"
	effortMedium effortLevel = "medium"
	effortHigh   effortLevel = "high"
	effortXHigh  effortLevel = "xhigh"
	effortMax    effortLevel = "max"

	// effortMinimal is Gemini's floor, not an Anthropic level. It is accepted
	// at the vocabulary boundary and clamped to effortLow; it never reaches the
	// wire. See effortClamps.
	effortMinimal effortLevel = "minimal"
)

// validEffortLevels is the closed set of accepted values.
var validEffortLevels = map[effortLevel]bool{
	effortLow:    true,
	effortMedium: true,
	effortHigh:   true,
	effortXHigh:  true,
	effortMax:    true,
}

// effortValues names Anthropic's own five levels for error messages, in the
// order the documentation lists them. These are the only values that ever reach
// the wire.
const effortValues = "low, medium, high, xhigh, max"

// effortAcceptedValues names the full vocabulary this provider *accepts* — the
// union of both providers' words. It is wider than effortValues by exactly the
// entries in effortClamps.
const effortAcceptedValues = "minimal, low, medium, high, xhigh, max"

// effortClamps translates an accepted-but-foreign reasoning-depth word onto the
// nearest native Anthropic level.
//
// The two vocabularies do not line up — Anthropic reads low|medium|high|xhigh|max
// and Gemini minimal|low|medium|high — and a `core.models` fanout role can
// dispatch one role to both providers at once. Each provider therefore accepts
// the union and clamps what it cannot express, rather than rejecting the other
// one's word (which would make such a role unconfigurable) or ignoring it
// (which would make `effort:` a silent no-op on this leg). The Gemini provider
// does the mirror image of this for `xhigh`/`max`.
//
// `minimal` clamps to `low`, which is lossless in spirit: `low` is already
// Anthropic's floor, so there is no shallower target to lose.
//
// This is deliberately *not* a model-capability table. It maps cross-*provider*
// vocabulary only; no entry here — and no code path anywhere in this provider —
// inspects the model id. `xhigh`/`max` remain invalid on some Claude models and
// that stays an Anthropic 400 the operator owns.
var effortClamps = map[effortLevel]effortLevel{
	effortMinimal: effortLow,
}

// validateEffort turns a raw string into a native effortLevel, clamping a value
// that belongs to the other provider's vocabulary and rejecting anything in
// neither with an error naming what is accepted.
//
// clamped reports that the returned level differs from the word supplied, so a
// caller can say so once in its logs. The clamp lives here, at the single
// vocabulary gate, rather than at the use sites. It *widens* the accepted set
// without making the provider permissive: a genuine typo is still an error.
//
// It is package-level rather than folded into parseOutputConfig so that a
// per-role effort (which arrives per request, not at Init) is validated against
// exactly the same vocabulary.
func validateEffort(v string) (lvl effortLevel, clamped bool, err error) {
	raw := effortLevel(v)
	if native, ok := effortClamps[raw]; ok {
		return native, true, nil
	}
	if !validEffortLevels[raw] {
		return effortUnset, false, fmt.Errorf("anthropic: effort %q is not a recognised reasoning-depth value (Anthropic: %s; Gemini: minimal, low, medium, high)", v, effortValues)
	}
	return raw, false, nil
}

// outputConfig is the parsed `output_config:` block of the plugin config. It
// mirrors the wire object of the same name, which also carries `format` for
// structured outputs and, in beta, `task_budget` — hence a block rather than a
// bare key, so those have an obvious home when they land.
type outputConfig struct {
	Effort effortLevel
}

// parseOutputConfig reads the optional `output_config:` block. An absent block,
// or a block with no `effort`, leaves Effort unset and nothing is emitted.
//
// Validation lives here so a bad level fails Init rather than the first request.
// A clamped level is warned here for the same reason: the provider-level key is
// a boot-time fact, so Init is the one place it needs saying.
func parseOutputConfig(cfg map[string]any, logger *slog.Logger) (outputConfig, error) {
	var out outputConfig
	if logger == nil {
		logger = slog.Default()
	}

	raw, ok := cfg["output_config"].(map[string]any)
	if !ok {
		return out, nil
	}

	v, present := raw["effort"]
	if !present {
		return out, nil
	}

	s, ok := v.(string)
	if !ok {
		return outputConfig{}, fmt.Errorf("anthropic: output_config.effort must be a string naming one of %s, got %T", effortAcceptedValues, v)
	}

	lvl, clamped, err := validateEffort(s)
	if err != nil {
		return outputConfig{}, fmt.Errorf("output_config.effort: %w", err)
	}
	if clamped {
		logger.Warn("output_config.effort clamped to Anthropic's effort range",
			"configured", s,
			"effective", string(lvl),
		)
	}
	out.Effort = lvl
	return out, nil
}

// applyEffort writes the resolved effort level into the request body's
// `output_config` object. An unset level writes nothing at all, so no
// `output_config` key is added by this path and the API default applies.
//
// The write is a **merge**, not an assignment: `output_config` is a shared wire
// object that also carries `format` (structured outputs) and beta
// `task_budget`. Nothing else in this provider writes it today — the native
// structured-output path still emits a stale top-level `response_format` — but
// when that is corrected, effort must not clobber it or be clobbered by it.
// For the same reason this runs last in buildRequestBody, after every other
// body field is in place.
func applyEffort(body map[string]any, effort effortLevel) {
	if effort == effortUnset {
		return
	}
	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		oc = make(map[string]any, 1)
	}
	oc["effort"] = string(effort)
	body["output_config"] = oc
}

// defaultRoleEffortSource labels an effort that came off the default
// `core.models` role rather than the one the request named. The registry
// exposes the default role's config but not its name, so it is described
// rather than quoted.
const defaultRoleEffortSource = "the default core.models role"

// effortSourceLabel names where a request's effort came from, for the error
// raised when it turns out to be invalid. The role name is the useful half of
// the answer whenever there is one: the value either came off that role's
// `core.models` entry or was set for that role upstream by the fallback or
// fanout coordinator.
func effortSourceLabel(role string) string {
	if role == "" {
		return "the request"
	}
	return fmt.Sprintf("core.models role %q", role)
}

// warnEffortClamped reports a clamped role effort, once per (role, configured
// value).
//
// A role's effort arrives per request rather than at Init, so request time is
// the only moment it can be said — but saying it on every request would flood
// the logs of a busy fanout role. The clamp is a configuration fact, so once is
// enough. The Gemini provider suppresses the same repetition for the same
// reason, there by walking the registry at Init instead.
func (p *Plugin) warnEffortClamped(role, configured string, effective effortLevel) {
	if _, seen := p.effortClampWarned.LoadOrStore(role+"\x00"+configured, struct{}{}); seen {
		return
	}
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("core.models effort clamped to Anthropic's effort range",
		"role", role,
		"configured", configured,
		"effective", string(effective),
	)
}
