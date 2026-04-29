package anthropic

import "log/slog"

// thinkingConfig controls Anthropic's extended-thinking feature (Sonnet 4+,
// Opus 4+). When enabled the API emits `thinking` (and possibly
// `redacted_thinking`) content blocks that carry the model's internal
// reasoning along with a cryptographic signature.
//
//	thinking:
//	  enabled: true
//	  budget_tokens: 8192     # -1 dynamic, 0 disabled (default 8192)
//	  include_thoughts: true  # surface human-readable thinking via thinking.step
//
// Constraints enforced by the API:
//   - budget_tokens >= 1024 and < max_tokens (caller's responsibility).
//   - temperature must be unset or 1.0; applyThinking strips non-1 values.
type thinkingConfig struct {
	Enabled         bool
	BudgetTokens    int  // -1 dynamic, 0 disabled (default 8192 when Enabled)
	IncludeThoughts bool // default true
}

// parseThinkingConfig pulls thinkingConfig out of the plugin's raw config map.
// Absent block returns a zero-value config (a no-op for applyThinking and the
// stream/sync handlers). Defaults BudgetTokens=8192 and IncludeThoughts=true
// when the block is present so users only have to set `enabled: true`.
func parseThinkingConfig(cfg map[string]any) thinkingConfig {
	tc := thinkingConfig{BudgetTokens: 8192, IncludeThoughts: true}

	raw, ok := cfg["thinking"].(map[string]any)
	if !ok {
		return thinkingConfig{}
	}

	if v, ok := raw["enabled"].(bool); ok {
		tc.Enabled = v
	}
	if v, ok := raw["budget_tokens"].(int); ok {
		tc.BudgetTokens = v
	} else if v, ok := raw["budget_tokens"].(float64); ok {
		// YAML decoders surface integers as float64 on some paths.
		tc.BudgetTokens = int(v)
	}
	if v, ok := raw["include_thoughts"].(bool); ok {
		tc.IncludeThoughts = v
	}

	return tc
}

// prependThinkingBlocks pulls a stashed []map[string]any of thinking blocks
// out of msg.Metadata["thinking_blocks"] and returns a fresh slice ready to
// be prepended to an assistant message's content array. Returns nil when no
// blocks are present or the value isn't the expected shape.
//
// Anthropic emits these blocks (with cryptographic signatures) as part of the
// response when extended thinking is on. On the NEXT assistant turn — the
// one that follows the tool result a tool_use produced — those exact blocks
// must be echoed back at the head of the content array, before the new
// tool_use blocks, with signatures intact. The API rejects with HTTP 400
// otherwise.
//
// We accept both []map[string]any (the in-memory shape providers use) and
// []any (post-JSON-roundtrip shape after the memory plugin persists +
// reloads via JSONL). The latter is best-effort: each element must itself
// be a map[string]any once Go decodes the JSON, which the standard library
// guarantees for object values.
func prependThinkingBlocks(meta map[string]any) []map[string]any {
	if meta == nil {
		return nil
	}
	raw, ok := meta["thinking_blocks"]
	if !ok {
		return nil
	}
	switch blocks := raw.(type) {
	case []map[string]any:
		// Defensive copy so callers can't mutate the cached slice.
		out := make([]map[string]any, len(blocks))
		copy(out, blocks)
		return out
	case []any:
		out := make([]map[string]any, 0, len(blocks))
		for _, b := range blocks {
			if m, ok := b.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

// applyThinking sets the request body's "thinking" field. When enabled it also
// strips temperature (Anthropic requires temp=1 with thinking; the cleanest
// path is to omit it). Logs a warning when stripping a user-set non-1 temp.
func applyThinking(body map[string]any, cfg thinkingConfig, logger *slog.Logger) {
	if !cfg.Enabled || cfg.BudgetTokens == 0 {
		return
	}
	body["thinking"] = map[string]any{
		"type":          "enabled",
		"budget_tokens": cfg.BudgetTokens,
	}
	if temp, ok := body["temperature"]; ok {
		// Only warn when the user set a value that conflicts with the
		// thinking-required temp=1. An exact 1.0 is silently dropped.
		if f, isFloat := temp.(float64); isFloat && f != 1.0 && logger != nil {
			logger.Warn("anthropic: stripping temperature for thinking-enabled request", "had", temp)
		}
		delete(body, "temperature")
	}
}
