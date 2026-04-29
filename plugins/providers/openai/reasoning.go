package openai

import (
	"log/slog"
	"regexp"
)

// reasoningConfig controls OpenAI o-series / gpt-5-thinking behavior.
//
//	reasoning:
//	  effort: medium     # minimal | low | medium | high
//	  include_summary: false
//
// Reasoning models reject temperature/top_p/etc., so the provider strips them
// automatically when isReasoningModel(model) is true.
type reasoningConfig struct {
	Effort         string
	IncludeSummary bool
}

// parseReasoningConfig extracts the reasoning sub-map from plugin config.
// Unknown effort values are silently ignored (left empty) so callers fall back
// to the model default.
func parseReasoningConfig(cfg map[string]any) reasoningConfig {
	rc := reasoningConfig{}
	raw, ok := cfg["reasoning"].(map[string]any)
	if !ok {
		return rc
	}
	if v, ok := raw["effort"].(string); ok {
		switch v {
		case "minimal", "low", "medium", "high":
			rc.Effort = v
		}
	}
	if v, ok := raw["include_summary"].(bool); ok {
		rc.IncludeSummary = v
	}
	return rc
}

// reasoningModelPattern matches o1*, o3*, o4* and gpt-5*-thinking* model ids.
// Match is conservative: known reasoning families only. Users can override
// detection with the `force_reasoning` config flag (handled in plugin.go).
var reasoningModelPattern = regexp.MustCompile(`^(o[134](-mini|-preview)?|gpt-5(-mini|-nano)?(-\d+)?(-thinking)?)`)

// isReasoningModel reports whether a model id is a known reasoning family.
func isReasoningModel(model string) bool {
	return reasoningModelPattern.MatchString(model)
}

// applyReasoning mutates the request body for reasoning-model calls.
// Strips disallowed fields (temperature, top_p, presence_penalty,
// frequency_penalty, logprobs, top_logprobs, prediction) and adds
// reasoning_effort when configured.
//
// NOTE: include_summary is documented as Responses-API only; we stay on
// /v1/chat/completions for now and ignore it. When/if Chat Completions gains
// support, surface it here.
func applyReasoning(body map[string]any, model string, cfg reasoningConfig, forceReasoning bool, logger *slog.Logger) {
	if !(forceReasoning || isReasoningModel(model)) {
		return
	}
	for _, f := range []string{
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"logprobs", "top_logprobs", "prediction",
	} {
		if _, ok := body[f]; ok {
			if logger != nil {
				logger.Debug("openai: stripping incompatible field for reasoning model", "field", f, "model", model)
			}
			delete(body, f)
		}
	}
	if cfg.Effort != "" {
		body["reasoning_effort"] = cfg.Effort
	}
}
