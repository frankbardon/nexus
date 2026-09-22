package anthropic

import "fmt"

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
)

// validEffortLevels is the closed set of accepted values.
var validEffortLevels = map[effortLevel]bool{
	effortLow:    true,
	effortMedium: true,
	effortHigh:   true,
	effortXHigh:  true,
	effortMax:    true,
}

// effortValues names the accepted values for error messages, in the order the
// documentation lists them.
const effortValues = "low, medium, high, xhigh, max"

// validateEffort turns a raw string into an effortLevel, rejecting anything
// outside the closed set with an error that names the accepted values.
//
// It is package-level rather than folded into parseOutputConfig so that a
// per-role effort (which arrives per request, not at Init) can be validated
// against exactly the same vocabulary.
func validateEffort(v string) (effortLevel, error) {
	lvl := effortLevel(v)
	if !validEffortLevels[lvl] {
		return effortUnset, fmt.Errorf("anthropic: effort %q is not one of %s", v, effortValues)
	}
	return lvl, nil
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
func parseOutputConfig(cfg map[string]any) (outputConfig, error) {
	var out outputConfig

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
		return outputConfig{}, fmt.Errorf("anthropic: output_config.effort must be a string naming one of %s, got %T", effortValues, v)
	}

	lvl, err := validateEffort(s)
	if err != nil {
		return outputConfig{}, fmt.Errorf("output_config.effort: %w", err)
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
