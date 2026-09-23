package engine

import (
	"sort"

	"github.com/frankbardon/nexus/pkg/events"
)

// Per-entry configuration on a `core.models` chain entry — effort, temperature,
// max_tokens, the API-surface selector and the native provider blocks — has to
// reach the provider that is actually serving *that* entry. A registry lookup
// cannot do it alone: ModelRegistry.Resolve always returns a role's first entry,
// so a fallback retry or a non-first fanout leg would silently be handed the
// primary's configuration.
//
// The mechanism, established by Effort and generalised here, has two halves:
//
//   - Coordinators that know which entry is being served stamp it onto the
//     outgoing request with StampModelConfig.
//   - Providers on the paths no coordinator touches — the role the request
//     names, the default role, and the late recovery after a router rewrote
//     `model` without touching the rest — recover it with ResolveModelConfig.
//
// One precedence rule governs both: a value already on the request wins
// outright, and an entry only fills an axis the request arrived without.

// StampModelConfig applies the chain entry a coordinator is serving to an
// outgoing request, filling only the axes the request does not already carry
// and marking the request as stamped.
//
// Callers are the fallback and fanout coordinators. Model is deliberately not
// touched: a coordinator retargets a request at a different model outright
// rather than filling a gap, so that assignment stays at the call site.
//
// The stamp is authoritative. Once it is set, ResolveModelConfig will not
// consult the registry again, because the registry would answer with the role's
// first entry rather than the one being served.
func StampModelConfig(req *events.LLMRequest, cfg ModelConfig) {
	if req == nil {
		return
	}
	applyModelConfig(req, cfg)
	req.Overrides.Stamped = true
}

// ResolveModelConfig returns a copy of req with every per-entry axis it does not
// already carry filled in from the model registry, and is how a provider
// recovers a role's configuration on the paths no coordinator touches.
//
// Layering, most specific first:
//
//  1. whatever the request already carries — a coordinator's stamp, an agent
//     posture's temperature, a gate's override;
//  2. the entry for the role the request names, when it names one. This is also
//     the late-recovery path: it is consulted whether or not `model` was already
//     set, so a router that rewrote `model` and left `role` alone does not
//     silently drop the role's configuration;
//  3. the default role's entry — for the shared axes only.
//
// Step 3 is deliberately narrower than step 2. The *shared* axes — max_tokens,
// effort and temperature — speak one vocabulary every provider understands, and
// an axis a named role leaves unset falling through to the default role is the
// long-standing Effort behaviour this generalises. The *provider-native* axes —
// the `thinking`, `reasoning`, `cache` and `retry` blocks and the `api` selector
// — do not fall through: they resolve from the role the request actually names,
// and the default role supplies them only when the request names no role at all,
// which is what "the default role" means.
//
// Without that split a deployment whose default role is Anthropic with
// `thinking: {mode: adaptive}` would silently hand an Anthropic-shaped block to
// a named Gemini role that was never given one — a configuration the operator
// did not write, in a vocabulary that provider does not speak.
//
// A stamped request is returned unchanged — the coordinator already supplied the
// only entry that is correct for it.
//
// Resolution says nothing about *which* provider should answer: a role naming
// another provider still resolves here. Callers do their own `_target_provider`
// and resolved-provider checks first, as every provider plugin already does.
func ResolveModelConfig(models *ModelRegistry, req events.LLMRequest) events.LLMRequest {
	if models == nil || req.Overrides.Stamped {
		return req
	}
	if req.Role != "" {
		if cfg, ok := models.Resolve(req.Role); ok {
			applyModelConfig(&req, cfg)
		}
		// The named role owns the provider-native axes outright, so only the
		// shared ones fall through.
		applySharedModelConfig(&req, models.Default())
		return req
	}
	applyModelConfig(&req, models.Default())
	return req
}

// applyModelConfig is the full gap-fill both halves are built from: every axis
// cfg sets and req does not is copied across, and nothing req already carries is
// disturbed.
func applyModelConfig(req *events.LLMRequest, cfg ModelConfig) {
	applySharedModelConfig(req, cfg)
	applyNativeModelConfig(req, cfg)
}

// applySharedModelConfig fills the axes whose vocabulary is shared across every
// provider: a token ceiling, a reasoning-depth word and a sampling temperature
// mean the same thing whoever ends up serving the request. These are the axes
// that may fall through from a named role to the default role.
func applySharedModelConfig(req *events.LLMRequest, cfg ModelConfig) {
	if cfg.MaxTokens > 0 && req.MaxTokens == 0 {
		req.MaxTokens = cfg.MaxTokens
	}
	if cfg.Effort != "" && req.Effort == "" {
		req.Effort = cfg.Effort
	}
	if cfg.Temperature != nil && req.Temperature == nil {
		// Copy the value rather than the pointer: ModelConfig is shared with
		// the loaded configuration and a request must not be able to reach it.
		t := *cfg.Temperature
		req.Temperature = &t
	}
}

// applyNativeModelConfig fills the axes written in one provider's own
// vocabulary — the native blocks and the API-surface selector. They belong to
// the entry that named them and never fall through to a different role's entry,
// because a different role may well be served by a different provider.
func applyNativeModelConfig(req *events.LLMRequest, cfg ModelConfig) {
	if cfg.API != "" && req.Overrides.API == "" {
		req.Overrides.API = cfg.API
	}
	// A nil block means the request said nothing about that axis, which is what
	// makes a gap; a set-but-empty block is a statement and blocks the fill.
	if req.Overrides.Thinking == nil {
		req.Overrides.Thinking = cloneModelBlock(cfg.Thinking)
	}
	if req.Overrides.Reasoning == nil {
		req.Overrides.Reasoning = cloneModelBlock(cfg.Reasoning)
	}
	if req.Overrides.Cache == nil {
		req.Overrides.Cache = cloneModelBlock(cfg.Cache)
	}
	if req.Overrides.Retry == nil {
		req.Overrides.Retry = cloneModelBlock(cfg.Retry)
	}
}

// cloneModelBlock copies a native provider block's top level so a request never
// aliases the map a ModelConfig holds — a stamped request travels the bus and
// outlives the call, and the registry's copy is shared by every request that
// resolves the same entry. Nested values stay shared: they are read-only by the
// ModelConfig contract, and a consumer that needs to change one builds its own
// map.
//
// nil in, nil out: "the entry said nothing" must not become "the entry set an
// empty block", because the two differ once a role block is merged over a
// plugin-level one.
func cloneModelBlock(block map[string]any) map[string]any {
	if block == nil {
		return nil
	}
	out := make(map[string]any, len(block))
	for k, v := range block {
		out[k] = v
	}
	return out
}

// WalkRoleEntries calls fn once for every `core.models` chain entry that
// providerID could be asked to serve, and stops at the first error fn returns.
//
// It is the skeleton every provider's Init-time `validateRole*` sweep is built
// from, and it exists because those sweeps all have to agree about four
// non-obvious things:
//
//   - Roles are walked in sorted order, so a config with several broken roles
//     fails on the same one every boot rather than on whichever one the map
//     iteration reached first.
//   - The whole chain is walked, not just the primary: a non-first entry's
//     configuration reaches a provider through the fallback or fanout
//     coordinator's stamp and is just as capable of being wrong.
//   - An entry naming another provider is that provider's business and is
//     skipped; an entry naming none may land anywhere, so it is checked.
//   - A nil registry is not an error — an embedder may run without one.
//
// The per-axis part — which block to read off the entry, what to do with it —
// stays with the provider, which is the only thing that can parse its own
// vocabulary.
func WalkRoleEntries(models *ModelRegistry, providerID string, fn func(role string, cfg ModelConfig) error) error {
	if models == nil || fn == nil {
		return nil
	}

	roles := models.Roles()
	sort.Strings(roles)

	for _, role := range roles {
		for i := 0; i < models.ChainLen(role); i++ {
			cfg, ok := models.Fallback(role, i)
			if !ok {
				continue
			}
			if cfg.Provider != "" && cfg.Provider != providerID {
				continue
			}
			if err := fn(role, cfg); err != nil {
				return err
			}
		}
	}
	return nil
}
