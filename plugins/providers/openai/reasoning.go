package openai

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
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

	// EffortSource records which configuration layer named Effort, which is
	// what decides whether it outranks a role's `effort:`. The zero value is
	// effortFromPlugin, so every plain parseReasoningConfig result — including
	// the plugin-level one Init stores — is correctly marked as the least
	// specific layer.
	EffortSource effortSource
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

// --- per-role reasoning -----------------------------------------------------

// effortSource names the configuration layer a reasoningConfig.Effort came
// from. Only two of the layers can supply one, and which of the two it was is
// exactly the question effortFor asks.
//
// It mirrors levelSource on nexus.llm.gemini, for the same reason: the parser
// sees a merged block and cannot tell which half of it named `effort`.
type effortSource int

const (
	// effortFromPlugin: the plugin-level `reasoning.effort`. The least
	// specific statement of depth there is, so a role's `effort:` outranks it.
	// It is the zero value, so every plain parseReasoningConfig result —
	// including the plugin-level one Init stores — is correctly marked as the
	// least specific layer.
	effortFromPlugin effortSource = iota

	// effortFromRole: the `effort` key of the `reasoning:` block on the
	// request — the `core.models` entry the request resolved to, or the one a
	// coordinator stamped for the chain entry actually being served. More
	// specific than the same role's `effort:`, and written in this provider's
	// own vocabulary besides.
	effortFromRole
)

// rawReasoningBlock lifts the plugin-level `reasoning:` block off the plugin
// config so it can be merged with a `core.models` role's block later. nil means
// the plugin set no block at all, which parseReasoningConfig reads as mode off.
//
// The map is not copied: everything reachable from it is treated as read-only,
// and mergeReasoningBlock always builds a fresh map rather than writing into
// either input.
func rawReasoningBlock(cfg map[string]any) map[string]any {
	block, ok := cfg["reasoning"].(map[string]any)
	if !ok {
		return nil
	}
	return block
}

// mergeReasoningBlock merges a `core.models` role's `reasoning:` block over the
// plugin-level one, key by key. The role wins on every key it mentions, and a
// plugin-level key the role is silent about survives — so a role that only
// wants a deeper `effort` does not have to restate the mode.
//
// A nil role block means the role said nothing, and the plugin block stands
// unchanged. A set-but-empty role block (`reasoning: {}`) is a statement rather
// than a gap: it overrides no individual key, but the merged block is non-nil,
// so a role can declare the target a reasoning model (parseReasoningConfig
// reads a present block with no mode as mode: effort) on a deployment whose
// plugin block is absent entirely.
//
// There is no mutually-exclusive key pair to special-case the way
// nexus.llm.gemini has to: OpenAI's reasoning controls are a single `effort`
// dial, so a plain key-wise merge is the whole story.
//
// Neither input is mutated and the result aliases neither: the plugin config
// map and the registry's block are both shared with the loaded configuration.
func mergeReasoningBlock(plugin, role map[string]any) map[string]any {
	if role == nil {
		return plugin
	}
	merged := make(map[string]any, len(plugin)+len(role))
	for k, v := range plugin {
		merged[k] = v
	}
	for k, v := range role {
		merged[k] = v
	}
	return merged
}

// parseMergedReasoning runs a merged block back through parseReasoningConfig —
// the one parser — by handing it the block wrapped in the shape it expects. A
// merged block is an ordinary `reasoning:` block and gets exactly the same
// validation, mode inference and deprecation handling the plugin-level one
// does. That also guarantees the result always carries a Mode, which
// applyReasoning's gate depends on.
//
// The one thing the parser cannot see is *which* layer named `effort`, so that
// is recorded here: only an effort the role itself named outranks the role's
// `effort:`.
//
// A nil logger suppresses the deprecation warnings, which is what the
// per-request path wants: validateRoleReasoning has already run every role's
// merged block through here once at Init with the real logger attached.
func parseMergedReasoning(plugin, role map[string]any, logger *slog.Logger) (reasoningConfig, error) {
	merged := mergeReasoningBlock(plugin, role)
	if merged == nil {
		return reasoningConfig{Mode: reasoningModeOff}, nil
	}
	rc, err := parseReasoningConfig(map[string]any{"reasoning": merged}, logger)
	if err != nil {
		return rc, err
	}
	if _, named := role["effort"]; named && rc.Effort != "" {
		rc.EffortSource = effortFromRole
	}
	return rc, nil
}

// effortFor picks the reasoning depth for one request.
//
// Precedence is the unified rule — specificity first:
//
//  1. an `effort` named by the request's own `reasoning:` block, which is
//     either the serving chain entry's block stamped by a coordinator or the
//     named role's block recovered from the registry;
//  2. the role's `effort:` (events.LLMRequest.Effort), which is likewise
//     either stamped or recovered;
//  3. the plugin-level `reasoning.effort`.
//
// No clamp and no warning: reasoningEfforts is a superset of the union of what
// nexus.llm.anthropic and nexus.llm.gemini accept, so every word an operator
// can write for either of those passes through verbatim. A word in no
// provider's vocabulary is a typo — validateRoleReasoning refuses the boot on
// one, and a request that carries one some other way falls through to the
// resolved block's own effort rather than putting a value OpenAI will 400 on
// onto the wire.
//
// An empty result means "send no reasoning_effort" and let the model's own
// default depth stand.
func effortFor(rc reasoningConfig, effort string) string {
	// An effort the role named outranks its own `effort:`, so an unrecognised
	// one cannot disturb a request whose depth was never going to read it.
	if rc.EffortSource == effortFromRole && rc.Effort != "" {
		return rc.Effort
	}
	if effort != "" && validReasoningEffort(effort) {
		return effort
	}
	return rc.Effort
}

// resolveReasoning picks the reasoning configuration for one request, with the
// depth already resolved into its Effort field.
//
// This is the single lookup point for the reasoning block, deliberately
// separate from its use site. Precedence, most specific first:
//
//  1. req.Overrides.Reasoning — the block of the chain entry actually being
//     served. It arrives either stamped by the fallback or fanout coordinator
//     (which alone knows a non-first entry is in play) or recovered from the
//     registry by engine.ResolveModelConfig on the paths no coordinator
//     touches. Either way it is merged over, not substituted for, the plugin
//     block.
//  2. the role's `effort:`, which becomes the `reasoning_effort` only where the
//     resolved block leaves room for it — see effortFor.
//  3. the plugin-level `reasoning:` block, which is what a request carrying no
//     override of its own gets, unchanged.
//
// The `mode` is the declaration throughout: a role's `effort:` on a deployment
// where nothing declares a reasoning mode sends nothing at all, exactly as it
// does under mode: off on nexus.llm.gemini. A bare depth cannot imply that the
// target is a reasoning model — this provider holds no model-capability table,
// `reasoning_effort` is an HTTP 400 on a model that is not one, and `effort` is
// a shared axis a role may be carrying for a different provider's entry in the
// same chain.
//
// An unparseable merged block is unreachable through Init, which sweeps every
// role this provider could serve and refuses to boot on one. The degradation
// here exists for a request whose Overrides.Reasoning was hand-set by something
// other than the registry: fall back to the plugin-level configuration rather
// than putting a shape OpenAI will 400 on onto the wire.
func (p *Plugin) resolveReasoning(req events.LLMRequest) reasoningConfig {
	rc := p.reasoning
	if req.Overrides.Reasoning != nil {
		// Nil logger: the warnings are boot-time facts, already said once by
		// validateRoleReasoning. Repeating them per request would flood the
		// log of a busy role.
		merged, err := parseMergedReasoning(p.reasoningRaw, req.Overrides.Reasoning, nil)
		if err != nil {
			logger := p.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("openai: ignoring an invalid per-request reasoning block; using the plugin-level configuration",
				"role", req.Role,
				"error", err,
			)
		} else {
			rc = merged
		}
	}
	rc.Effort = effortFor(rc, req.Effort)
	return rc
}

// validateRoleReasoning checks every `core.models` role this provider could
// serve, at Init, across both halves of what a role entry can say about
// reasoning depth: its native `reasoning:` block and its `effort:`.
//
// The two are checked together because they cannot be checked apart. Whether a
// role's `effort` can reach the wire at all depends on the mode and the effort
// of the block that role resolves to, and with per-role blocks that is no
// longer the plugin-level configuration — it is the role's block merged over
// it.
//
// Blocks: a block on a `core.models` entry bypasses the plugin's schema.json
// entirely — core stores these maps without looking inside them — so nothing
// else ever checks them. Without this sweep a role whose merged block is
// invalid (`mode: thorough`, say) would boot clean and fail at the first
// request that named it, possibly hours later. The error names the role, which
// is the useful half of the answer.
//
// Efforts: there is no clamp on this provider, so there is nothing to warn
// about — every word in the Anthropic ∪ Gemini union is already in OpenAI's own
// vocabulary. What is left is the typo, and it fails the boot. Validation runs
// only where the effort can actually reach the wire: under mode: effort, and
// only when the role's own block did not name an `effort` to outrank it. A role
// whose effort is inert here is logged at debug and nothing more — that is the
// ordinary shape of a chain whose other entries are served by another provider.
//
// The sweep itself — sorted roles, whole chain, foreign entries skipped — is
// engine.WalkRoleEntries; see there for why each of those matters.
//
// pluginRC is the already-parsed plugin-level configuration, used verbatim for
// every entry that carries no block of its own. Re-parsing the plugin block per
// role would repeat its deprecation warnings once per role; Init has already
// said them once.
func validateRoleReasoning(models *engine.ModelRegistry, plugin map[string]any, pluginRC reasoningConfig, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	return engine.WalkRoleEntries(models, pluginID, func(role string, cfg engine.ModelConfig) error {
		rc := pluginRC
		if cfg.Reasoning != nil {
			// The role's logger, so a deprecation warning raised by the merged
			// block says which role raised it. Only entries that actually set
			// a block get here, so the plugin block's own warnings — already
			// said once by Init — are not repeated per role.
			merged, err := parseMergedReasoning(plugin, cfg.Reasoning, logger.With("role", role))
			if err != nil {
				return fmt.Errorf("core.models role %q: %w", role, err)
			}
			rc = merged
		}

		if cfg.Effort == "" {
			return nil
		}
		if rc.Mode != reasoningModeEffort {
			logger.Debug("openai: core.models effort is inert on this entry — no reasoning mode is declared for it",
				"role", role,
				"effort", cfg.Effort,
			)
			return nil
		}
		if rc.EffortSource == effortFromRole && rc.Effort != "" {
			return nil
		}
		if !validReasoningEffort(cfg.Effort) {
			return fmt.Errorf("core.models role %q: effort %q is not one of %s",
				role, cfg.Effort, strings.Join(reasoningEfforts, ", "))
		}
		return nil
	})
}
