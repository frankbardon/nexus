package anthropic

import (
	"fmt"
	"log/slog"
)

// thinkingMode is the operator-declared wire shape for Anthropic's
// extended-thinking field. There is deliberately no model-capability table
// here: the provider never inspects the model id to pick a shape. A mode the
// target model does not accept is an Anthropic HTTP 400, and the operator owns
// that mismatch.
type thinkingMode string

const (
	// thinkingModeOff omits the `thinking` field from the request body
	// entirely. This is how thinking is turned off on models that do not
	// think unless asked (Opus 4.8, Opus 4.7 and older).
	thinkingModeOff thinkingMode = "off"

	// thinkingModeAdaptive sends {"type":"adaptive"} — the model decides how
	// much to think. The only on-mode on the current model families.
	thinkingModeAdaptive thinkingMode = "adaptive"

	// thinkingModeBudget sends {"type":"enabled","budget_tokens":N} — the
	// legacy fixed-budget shape, accepted only by Sonnet 4.5 / Haiku 4.5 and
	// older, plus Opus 4.6 / Sonnet 4.6 as a deprecated escape hatch.
	thinkingModeBudget thinkingMode = "budget"

	// thinkingModeDisabled sends {"type":"disabled"} — an explicit opt out,
	// and the only way off on models that think by default (Opus 5, Sonnet 5).
	// Distinct from thinkingModeOff, which sends nothing at all.
	thinkingModeDisabled thinkingMode = "disabled"
)

// thinkingDisplay is the value of the `thinking.display` field, which controls
// whether the API returns readable reasoning text. It is a visibility control
// only: whether thinking happens, and how it is billed, is identical under
// every value, and the raw chain of thought is never exposed on any model.
//
// The API default is model-dependent and Nexus never infers it from the model
// id — see thinkingConfig.Display for the one inference this provider does make.
type thinkingDisplay string

const (
	// thinkingDisplayUnset leaves the field off the request body entirely, so
	// whatever the target model defaults to applies.
	thinkingDisplayUnset thinkingDisplay = ""

	// thinkingDisplaySummarized returns a readable summary of the model's
	// reasoning in the `thinking` blocks. The default on Opus 4.6 / Sonnet 4.6
	// and older; it must be asked for on the current families.
	thinkingDisplaySummarized thinkingDisplay = "summarized"

	// thinkingDisplayOmitted still streams `thinking` blocks, but with empty
	// text. The API default on Fable 5/5.1, Opus 5, Opus 4.8, Opus 4.7 and
	// Sonnet 5.
	thinkingDisplayOmitted thinkingDisplay = "omitted"
)

// thinkingConfig controls Anthropic's extended-thinking feature. When thinking
// is on the API emits `thinking` (and possibly `redacted_thinking`) content
// blocks that carry the model's internal reasoning along with a cryptographic
// signature.
//
//	thinking:
//	  mode: adaptive          # adaptive | budget | disabled | off
//	  budget_tokens: 8192     # required when mode: budget, ignored otherwise
//	  display: summarized     # summarized | omitted; inferred from include_thoughts
//	  include_thoughts: true  # surface human-readable thinking via thinking.step
//
// Constraints enforced by the API, not by this provider:
//   - budget_tokens >= 1024 and < max_tokens (caller's responsibility).
//   - temperature must be unset or 1.0; applyThinking strips non-1 values.
type thinkingConfig struct {
	Mode            thinkingMode
	BudgetTokens    int             // only meaningful when Mode is thinkingModeBudget
	Display         thinkingDisplay // "" means send no display key at all
	IncludeThoughts bool            // default true when the thinking block is present
}

// parseThinkingConfig pulls thinkingConfig out of the plugin's raw config map.
// An absent block resolves to thinkingModeOff; a present block with no `mode`
// resolves to thinkingModeAdaptive. Validation that can fail lives here so a
// bad config fails Init rather than the first request.
//
// The legacy `enabled` bool is still accepted as a deprecated alias
// (true → adaptive, false → off) and is ignored when `mode` is set. A non-zero
// `budget_tokens` with no `mode` infers thinkingModeBudget, which preserves the
// one legacy configuration that still works on the wire (Opus 4.6 / Sonnet 4.6);
// `budget_tokens: 0` with no `mode` keeps its long-documented meaning of
// "disable thinking" and infers thinkingModeOff, because inferring budget there
// would put an unconditional HTTP 400 (`budget_tokens: 0`) on the wire.
//
// `display` is a pass-through with one deliberate inference: when it is unset
// and `include_thoughts` resolves true, thinkingDisplaySummarized is used for
// the modes that actually think. Without that, `include_thoughts` becomes a
// silent no-op on every model whose `display` default is `omitted` — the API
// still streams `thinking` blocks, but with empty text, so the events carry
// nothing and a UI just shows a long silent pause. An explicit `display` always
// wins, in both directions.
func parseThinkingConfig(cfg map[string]any, logger *slog.Logger) (thinkingConfig, error) {
	raw, ok := cfg["thinking"].(map[string]any)
	if !ok {
		return thinkingConfig{Mode: thinkingModeOff}, nil
	}

	tc := thinkingConfig{IncludeThoughts: true}

	if v, ok := raw["include_thoughts"].(bool); ok {
		tc.IncludeThoughts = v
	}

	displaySet := false
	if v, ok := raw["display"].(string); ok {
		switch thinkingDisplay(v) {
		case thinkingDisplaySummarized, thinkingDisplayOmitted:
			tc.Display = thinkingDisplay(v)
			displaySet = true
		default:
			return thinkingConfig{}, fmt.Errorf("anthropic: thinking.display %q is not one of summarized, omitted", v)
		}
	}

	budgetSet := false
	if v, ok := raw["budget_tokens"].(int); ok {
		tc.BudgetTokens = v
		budgetSet = true
	} else if v, ok := raw["budget_tokens"].(float64); ok {
		// YAML decoders surface integers as float64 on some paths.
		tc.BudgetTokens = int(v)
		budgetSet = true
	}

	enabled, enabledSet := raw["enabled"].(bool)

	modeRaw, modeSet := raw["mode"].(string)
	switch {
	case modeSet:
		switch thinkingMode(modeRaw) {
		case thinkingModeOff, thinkingModeAdaptive, thinkingModeBudget, thinkingModeDisabled:
			tc.Mode = thinkingMode(modeRaw)
		default:
			return thinkingConfig{}, fmt.Errorf("anthropic: thinking.mode %q is not one of adaptive, budget, disabled, off", modeRaw)
		}
		if enabledSet && logger != nil {
			logger.Warn("anthropic: thinking.enabled is deprecated and ignored when thinking.mode is set; remove it",
				"mode", modeRaw,
				"enabled", enabled,
			)
		}
	case enabledSet && !enabled:
		// An explicit opt out wins over any budget left behind in the block.
		tc.Mode = thinkingModeOff
	case budgetSet && tc.BudgetTokens == 0:
		// `budget_tokens: 0` has always meant "disable thinking" on this
		// provider, and still does. Inferring mode: budget here would send
		// {"type":"enabled","budget_tokens":0}, which every model rejects.
		tc.Mode = thinkingModeOff
		if logger != nil {
			logger.Warn("anthropic: thinking.budget_tokens: 0 with no thinking.mode means thinking is off; use thinking.mode: off (or disabled) instead")
		}
	case budgetSet:
		// The legacy fixed-budget shape. Honour it, but say so out loud —
		// budget_tokens is an HTTP 400 on every current model family.
		tc.Mode = thinkingModeBudget
		if logger != nil {
			logger.Warn("anthropic: thinking.budget_tokens with no thinking.mode infers mode: budget; set thinking.mode explicitly",
				"budget_tokens", tc.BudgetTokens,
			)
		}
	default:
		// A present block with no mode — and the deprecated `enabled: true` —
		// both resolve to adaptive.
		tc.Mode = thinkingModeAdaptive
	}

	if enabledSet && !modeSet && logger != nil {
		logger.Warn("anthropic: thinking.enabled is deprecated; use thinking.mode (adaptive, budget, disabled, off)",
			"enabled", enabled,
			"resolved_mode", string(tc.Mode),
		)
	}

	if tc.Mode == thinkingModeBudget && !budgetSet {
		return thinkingConfig{}, fmt.Errorf("anthropic: thinking.mode: budget requires thinking.budget_tokens")
	}

	// include_thoughts infers display. Only for the modes that actually think:
	// the inference exists to keep thinking.step events carrying text, and
	// there is no reasoning to display under disabled or off. An explicit
	// display is never touched, in either direction.
	if !displaySet && tc.IncludeThoughts {
		switch tc.Mode {
		case thinkingModeAdaptive, thinkingModeBudget:
			tc.Display = thinkingDisplaySummarized
		}
	}

	return tc, nil
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

// applyThinking sets the request body's "thinking" field from the declared
// mode. thinkingModeOff writes nothing at all — the field is absent from the
// body, which is a different request from {"type":"disabled"} and the two are
// not interchangeable on the wire.
//
// A resolved Display rides inside whatever thinking object the mode builds; an
// unset one leaves the key off so the model's own default applies. Under
// thinkingModeOff there is no object to carry it, so it is dropped with the
// rest of the field.
//
// When thinking is actually on (adaptive or budget) it also strips temperature
// (Anthropic requires temp=1 with thinking; the cleanest path is to omit it)
// and logs a warning when dropping a user-set non-1 value.
func applyThinking(body map[string]any, cfg thinkingConfig, logger *slog.Logger) {
	var thinking map[string]any
	switch cfg.Mode {
	case thinkingModeAdaptive:
		thinking = map[string]any{"type": "adaptive"}
	case thinkingModeBudget:
		thinking = map[string]any{
			"type":          "enabled",
			"budget_tokens": cfg.BudgetTokens,
		}
	case thinkingModeDisabled:
		thinking = map[string]any{"type": "disabled"}
	default:
		// thinkingModeOff (and the zero value): no thinking field at all.
		return
	}

	if cfg.Display != thinkingDisplayUnset {
		thinking["display"] = string(cfg.Display)
	}
	body["thinking"] = thinking

	if cfg.Mode == thinkingModeDisabled {
		// Thinking is off, so the temperature constraint does not apply.
		return
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
