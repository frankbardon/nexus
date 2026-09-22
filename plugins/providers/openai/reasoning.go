package openai

import (
	"fmt"
	"log/slog"
	"strings"
)

// reasoningMode is the operator-declared statement of whether the target model
// is a reasoning model and should be sent reasoning controls.
//
// It mirrors the `mode` axis `nexus.llm.anthropic` and `nexus.llm.gemini`
// already carry: the operator declares the shape, the provider obeys, and a
// mode the target model does not accept is an OpenAI HTTP 400 the operator
// owns. There is deliberately only one on-mode today — OpenAI's reasoning
// controls are a single `effort` dial, with no budget-token counterpart.
type reasoningMode string

const (
	// reasoningModeOff sends no reasoning configuration at all. This is what
	// an absent `reasoning:` block resolves to.
	reasoningModeOff reasoningMode = "off"

	// reasoningModeEffort declares the target a reasoning model and sends the
	// configured effort. An effort left unset under this mode sends no
	// `reasoning_effort` key, so the model's own default applies.
	reasoningModeEffort reasoningMode = "effort"
)

// reasoningEfforts is OpenAI's full current effort vocabulary, in ascending
// order of depth.
//
// It is deliberately NOT clamped: this set is a superset of the union of what
// nexus.llm.anthropic (low…max) and nexus.llm.gemini (minimal…high) accept, so
// every word an operator can write for either of those passes through to
// OpenAI verbatim. Model-dependent rejections (GPT-6 Astra 400s on `none`) are
// the operator's to own, exactly as on the other two providers.
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// reasoningSummaries is the accepted set for the `summary` key — OpenAI's
// reasoning-summary verbosity dial.
var reasoningSummaries = []string{"auto", "concise", "detailed"}

func validReasoningEffort(v string) bool {
	for _, e := range reasoningEfforts {
		if e == v {
			return true
		}
	}
	return false
}

func validReasoningSummary(v string) bool {
	for _, s := range reasoningSummaries {
		if s == v {
			return true
		}
	}
	return false
}

// reasoningConfig controls OpenAI reasoning-model behavior.
//
//	reasoning:
//	  mode: effort       # effort | off
//	  effort: medium     # none | minimal | low | medium | high | xhigh | max
//	  summary: auto      # auto | concise | detailed
//
// Any mode other than `off` declares the target a reasoning model, so the
// provider strips the sampling parameters such a model rejects — see
// applyReasoning.
type reasoningConfig struct {
	Mode   reasoningMode
	Effort string // only meaningful when Mode is reasoningModeEffort
	// Summary is accepted and validated here but does not reach the wire on
	// the Chat Completions path, which has no reasoning-summary field at all.
	// It is the Responses API (`/v1/responses`) that returns summaries, and
	// this provider does not speak it yet.
	Summary string
}

// parseReasoningConfig pulls reasoningConfig out of the plugin's raw config
// map. An absent block resolves to reasoningModeOff; a present block with no
// `mode` resolves to reasoningModeEffort. Validation that can fail lives here
// so a bad config fails Init rather than the first request.
//
// Two deprecated keys are still accepted rather than failing the boot, because
// they are what the shipped schema.json declared:
//
//   - `enabled` is a deprecated alias for `mode` (true → effort, false → off),
//     ignored when `mode` is set. Mirrors how `thinking.enabled` is handled on
//     nexus.llm.anthropic and nexus.llm.gemini.
//   - `budget_tokens` has never had a wire counterpart on this provider —
//     OpenAI has no reasoning token budget, only `effort` — so it is accepted,
//     warned about and ignored.
func parseReasoningConfig(cfg map[string]any, logger *slog.Logger) (reasoningConfig, error) {
	raw, ok := cfg["reasoning"].(map[string]any)
	if !ok {
		return reasoningConfig{Mode: reasoningModeOff}, nil
	}

	rc := reasoningConfig{}

	if v, ok := raw["effort"].(string); ok {
		if !validReasoningEffort(v) {
			return reasoningConfig{}, fmt.Errorf("openai: reasoning.effort %q is not one of %s",
				v, strings.Join(reasoningEfforts, ", "))
		}
		rc.Effort = v
	}

	if v, ok := raw["summary"].(string); ok {
		if !validReasoningSummary(v) {
			return reasoningConfig{}, fmt.Errorf("openai: reasoning.summary %q is not one of %s",
				v, strings.Join(reasoningSummaries, ", "))
		}
		rc.Summary = v
	}

	if _, present := raw["budget_tokens"]; present && logger != nil {
		logger.Warn("openai: reasoning.budget_tokens is not a thing on this provider and is ignored; use reasoning.effort to set reasoning depth")
	}

	enabled, enabledSet := raw["enabled"].(bool)
	modeRaw, modeSet := raw["mode"].(string)

	switch {
	case modeSet:
		switch reasoningMode(modeRaw) {
		case reasoningModeEffort, reasoningModeOff:
			rc.Mode = reasoningMode(modeRaw)
		default:
			return reasoningConfig{}, fmt.Errorf("openai: reasoning.mode %q is not one of effort, off", modeRaw)
		}
		if enabledSet && logger != nil {
			logger.Warn("openai: reasoning.enabled is deprecated and ignored when reasoning.mode is set; remove it",
				"mode", modeRaw,
				"enabled", enabled,
			)
		}
	case enabledSet && !enabled:
		rc.Mode = reasoningModeOff
	default:
		// A present block with no mode — and the deprecated `enabled: true` —
		// both resolve to the one on-mode.
		rc.Mode = reasoningModeEffort
	}

	if enabledSet && !modeSet && logger != nil {
		logger.Warn("openai: reasoning.enabled is deprecated; use reasoning.mode (effort, off)",
			"enabled", enabled,
			"resolved_mode", string(rc.Mode),
		)
	}

	if rc.Summary != "" && logger != nil {
		logger.Warn("openai: reasoning.summary is accepted but does not reach the wire — /v1/chat/completions returns no reasoning summaries",
			"summary", rc.Summary,
		)
	}

	return rc, nil
}

// applyReasoning mutates the request body for reasoning-model calls.
// Strips disallowed fields (temperature, top_p, presence_penalty,
// frequency_penalty, logprobs, top_logprobs, prediction) and adds
// reasoning_effort when the declared mode asks for it.
//
// The gate is the operator's declared `reasoning.mode`, never the model id.
// This provider deliberately holds no model-capability table: a table cannot
// know about a model released after it was written, and a model it guesses
// wrong about produces an OpenAI HTTP 400 nobody can diagnose locally.
// Declaring a mode the target model does not accept is likewise an HTTP 400
// the operator owns — the same bargain nexus.llm.anthropic and
// nexus.llm.gemini already strike.
//
// reasoningModeOff — and an absent `reasoning:` block, which resolves to it —
// puts no reasoning configuration on the body at all and strips nothing.
func applyReasoning(body map[string]any, cfg reasoningConfig, logger *slog.Logger) {
	switch cfg.Mode {
	case "", reasoningModeOff:
		return
	}
	for _, f := range []string{
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"logprobs", "top_logprobs", "prediction",
	} {
		if _, ok := body[f]; ok {
			if logger != nil {
				logger.Debug("openai: stripping field a reasoning model rejects", "field", f, "mode", string(cfg.Mode))
			}
			delete(body, f)
		}
	}
	if cfg.Mode == reasoningModeEffort && cfg.Effort != "" {
		body["reasoning_effort"] = cfg.Effort
	}
}
