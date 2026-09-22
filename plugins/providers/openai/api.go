package openai

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// apiSurface names which OpenAI API a request is spoken to. It is an
// operator-declared axis rather than a capability this provider sniffs, for the
// same reason `reasoning.mode` is: only the operator knows what the endpoint on
// the other end of `base_url` actually implements.
//
// The two are not interchangeable and the choice is not cosmetic:
//
//   - From GPT-5.4 onward Chat Completions refuses tool calling with any
//     `reasoning_effort` other than `none`. Nexus puts tools on every turn, so
//     on a current reasoning model the Chat Completions path cannot reason at
//     all — `api: responses` is the only configuration that works there.
//   - `base_url` exists for proxies and OpenAI-compatible endpoints (vLLM,
//     Ollama, OpenRouter, LM Studio and friends), which implement
//     `/chat/completions` and mostly not `/responses`. Azure's Responses
//     surface is a different route shape again — not the deployment-scoped chat
//     one this provider builds today.
//
// So neither value can be right for everyone, and the endpoint is declared.
type apiSurface string

const (
	// apiChatCompletions is `/v1/chat/completions` — the surface this provider
	// has always spoken, and the one every OpenAI-compatible endpoint
	// implements.
	apiChatCompletions apiSurface = "chat_completions"

	// apiResponses is `/v1/responses`. Declared and validated here; the
	// request, reply and streaming path that speaks it is not implemented yet,
	// so Init refuses it rather than letting it fail on the wire. See
	// errResponsesUnimplemented.
	apiResponses apiSurface = "responses"
)

// apiSurfaces is the accepted vocabulary, for error messages.
var apiSurfaces = []string{string(apiChatCompletions), string(apiResponses)}

func validAPISurface(v string) bool {
	switch apiSurface(v) {
	case apiChatCompletions, apiResponses:
		return true
	}
	return false
}

// unnarrowedDefaultAPI is the effective `api` for a deployment that declares
// none and whose endpoint is plain `api.openai.com`.
//
// It is `chat_completions` **today only because the Responses path does not
// exist yet** — every explicit `api: responses` is an Init error (see
// errResponsesUnimplemented), so defaulting to it would mean a plain
// `api.openai.com` deployment with no `api:` key could not boot. When the
// Responses path lands this constant flips to apiResponses, which is the
// intended default: it is the only surface on which a current reasoning model
// can reason while tools are on the turn. The narrowing in
// narrowsToChatCompletions is already written for that flip and is what keeps
// declared-compat endpoints on the surface they actually implement.
const unnarrowedDefaultAPI = apiChatCompletions

// errResponsesUnimplemented is what an explicit `api: responses` gets until the
// Responses request/response/stream path exists. An honest boot failure naming
// the release beats a request shaped for one API and posted to another.
func errResponsesUnimplemented(where string) error {
	prefix := "openai: "
	if where != "" {
		prefix = fmt.Sprintf("openai: %s: ", where)
	}
	return fmt.Errorf("%sapi: responses is not implemented yet — the /v1/responses path lands in v0.29.0; use api: chat_completions (or omit the key) until then", prefix)
}

// narrowsToChatCompletions reports whether this deployment has declared an
// endpoint that cannot be assumed to speak the Responses API, which is what
// narrows the default.
//
// Two cases, both operator-declared:
//
//   - `base_url` is set. It is documented as the override for proxies and
//     OpenAI-compatible endpoints, and those implement `/chat/completions`
//     first and `/responses` rarely.
//   - an Azure auth mode. Azure's Responses surface is `/openai/v1/responses`
//     with the deployment in the body, not the deployment-scoped chat route
//     this provider builds — a different URL shape, with its own regional
//     availability.
//
// An operator on either who wants Responses says so with an explicit `api:`.
func (a *authState) narrowsToChatCompletions() bool {
	if a == nil {
		return false
	}
	if a.baseURL != "" {
		return true
	}
	return a.mode == authModeAzureKey || a.mode == authModeAzureAAD
}

// defaultAPI is the effective surface when nothing declares one — the narrowed
// default described on unnarrowedDefaultAPI.
func defaultAPI(a *authState) apiSurface {
	if a.narrowsToChatCompletions() {
		return apiChatCompletions
	}
	return unnarrowedDefaultAPI
}

// parseAPIConfig resolves the plugin-level `api:` key against the narrowed
// default. An unknown word and the not-yet-implemented `responses` both fail
// Init rather than the first request.
func parseAPIConfig(cfg map[string]any, auth *authState) (apiSurface, error) {
	raw, ok := cfg["api"].(string)
	if !ok || raw == "" {
		return defaultAPI(auth), nil
	}
	if !validAPISurface(raw) {
		return "", fmt.Errorf("openai: api %q is not one of %s", raw, strings.Join(apiSurfaces, ", "))
	}
	if apiSurface(raw) == apiResponses {
		return "", errResponsesUnimplemented("")
	}
	return apiSurface(raw), nil
}

// validateRoleAPI checks the `api:` of every `core.models` chain entry this
// provider could serve, at Init.
//
// A per-entry `api:` bypasses schema.json entirely — core stores the value
// without looking at it — so this sweep is the only thing that checks it, and
// it checks it at boot rather than at the first request that names the role.
// The sweep itself (sorted roles, whole chain, foreign entries skipped) is
// engine.WalkRoleEntries.
func validateRoleAPI(models *engine.ModelRegistry) error {
	return engine.WalkRoleEntries(models, pluginID, func(role string, cfg engine.ModelConfig) error {
		if cfg.API == "" {
			return nil
		}
		if !validAPISurface(cfg.API) {
			return fmt.Errorf("core.models role %q: api %q is not one of %s",
				role, cfg.API, strings.Join(apiSurfaces, ", "))
		}
		if apiSurface(cfg.API) == apiResponses {
			return errResponsesUnimplemented(fmt.Sprintf("core.models role %q", role))
		}
		return nil
	})
}

// resolveAPI picks the API surface for one request. Exactly one surface is
// chosen here and nothing downstream reconsiders it.
//
// Precedence is the engine-wide one — specificity first:
//
//  1. req.Overrides.API — the `api:` of the chain entry actually being served,
//     either stamped by the fallback or fanout coordinator or recovered from
//     the registry by engine.ResolveModelConfig. `api` is a provider-native
//     axis, so unlike the shared ones it never falls through from the default
//     role to a named one;
//  2. the plugin-level `api:`;
//  3. the narrowed default, which parseAPIConfig has already folded into (2).
//
// An unrecognised override is a value nothing on this provider can honour, so
// it is warned about and dropped back to the plugin's — the same degradation
// resolveReasoning and resolveRetry make. `responses` is *not* dropped: it is
// recognised, and resolveEndpoint refuses it by name, which is the honest
// failure while the path is unimplemented.
func (p *Plugin) resolveAPI(req events.LLMRequest) apiSurface {
	if v := req.Overrides.API; v != "" {
		if validAPISurface(v) {
			return apiSurface(v)
		}
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("openai: ignoring an unrecognised per-request api; using the plugin-level one",
			"role", req.Role,
			"api", v,
			"using", string(p.api),
			"accepted", strings.Join(apiSurfaces, ", "),
		)
	}
	return p.api
}

// firstChatCompletionsEntry finds the first place a deployment both speaks the
// Chat Completions API and has a reasoning configuration matching want: the
// plugin-level statement if it qualifies, else the first `core.models` role in
// sorted order that does.
//
// It exists because the boot-time statements about what the chat surface cannot
// do are each worth making exactly once, not once per role, and because the
// question "which surface does this entry resolve to, and what reasoning
// configuration does it resolve to" has one answer shared by all of them.
//
// The returned role is empty when the plugin-level configuration was the match,
// which is the one case that has no role to name. The bool is the only thing
// that reports whether anything matched at all.
func (p *Plugin) firstChatCompletionsEntry(want func(reasoningConfig) bool) (string, bool) {
	if p.api == apiChatCompletions && want(p.reasoning) {
		return "", true
	}

	var role string
	found := false
	_ = engine.WalkRoleEntries(p.models, pluginID, func(r string, cfg engine.ModelConfig) error {
		if found {
			return nil
		}
		api := p.api
		if cfg.API != "" && validAPISurface(cfg.API) {
			api = apiSurface(cfg.API)
		}
		if api != apiChatCompletions {
			return nil
		}
		rc := p.reasoning
		if cfg.Reasoning != nil {
			// Nil logger: validateRoleReasoning has already said this block's
			// deprecation warnings once, naming the role.
			merged, err := parseMergedReasoning(p.reasoningRaw, cfg.Reasoning, nil)
			if err != nil {
				return nil
			}
			rc = merged
		}
		if !want(rc) {
			return nil
		}
		role, found = r, true
		return nil
	})
	return role, found
}

// warnReasoningOnChatCompletions says once, at Init, that a deployment has
// configured reasoning it will not get.
//
// From GPT-5.4 onward Chat Completions refuses tool calling with any
// `reasoning_effort` other than `none`, and Nexus puts tools on every turn — so
// on a current reasoning model the configured depth is not merely degraded, it
// is an HTTP 400 or an ignored dial depending on the model. The provider holds
// no model-capability table and will not guess whether the target is one of the
// affected models; it states the restriction once and names the fix.
//
// Once means once: the plugin-level statement is checked first and, failing
// that, the roles are swept in sorted order and the first entry that reasons on
// the chat surface raises the warning.
func (p *Plugin) warnReasoningOnChatCompletions() {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	const msg = "openai: reasoning is configured but this deployment speaks the Chat Completions API — from GPT-5.4 onward Chat Completions refuses tool calling with any reasoning_effort other than `none`, and Nexus puts tools on every turn; declare `api: responses` to use the Responses API"

	role, found := p.firstChatCompletionsEntry(func(rc reasoningConfig) bool {
		return rc.Mode == reasoningModeEffort
	})
	if !found {
		return
	}
	attrs := []any{"api", string(apiChatCompletions)}
	if role != "" {
		attrs = append(attrs, "role", role)
	}
	logger.Warn(msg, attrs...)
}

// warnSummaryOnChatCompletions says once, at Init, that a configured
// `reasoning.summary` will not reach the wire on this deployment.
//
// The summary verbosity dial is a Responses-API field: that surface takes a
// `reasoning` object carrying both `effort` and `summary`, while Chat
// Completions has only the `reasoning_effort` scalar and no summary field at
// all. So the warning is about the *surface*, not about the key — which is why
// it lives here rather than in parseReasoningConfig, where it used to fire
// unconditionally. A deployment on `api: responses` configures a summary and
// gets one; warning it would be a lie.
//
// Same "once" discipline as the restriction warning above, and the same sweep:
// the plugin-level statement first, then the roles in sorted order.
func (p *Plugin) warnSummaryOnChatCompletions() {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	const msg = "openai: reasoning.summary does not reach the wire on this deployment — /v1/chat/completions returns no reasoning summaries; declare `api: responses` to receive them"

	role, found := p.firstChatCompletionsEntry(func(rc reasoningConfig) bool {
		return rc.Summary != ""
	})
	if !found {
		return
	}
	attrs := []any{"api", string(apiChatCompletions)}
	if role != "" {
		attrs = append(attrs, "role", role)
	}
	logger.Warn(msg, attrs...)
}
