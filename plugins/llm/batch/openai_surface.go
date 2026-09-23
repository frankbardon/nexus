package batch

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file answers one question for a batched OpenAI request: which OpenAI API
// is this line addressed to, and what reasoning configuration rides on it.
//
// It exists because the coordinator used to answer neither. Every JSONL line
// was `url: "/v1/chat/completions"` and every body came out of a builder that
// had never heard of `reasoning`, so a `core.models` role configured for depth
// lost that configuration the moment it was batched — silently, and in the one
// direction that matters: from GPT-5.4 onward Chat Completions refuses tool
// calling with any `reasoning_effort` other than `none`.
//
// WHAT IS SHARED WITH nexus.llm.openai AND WHAT IS NOT. The *resolution* is
// shared and is the important half: the per-entry axes reach this plugin
// through engine.ResolveModelConfig, the same core mechanism the provider uses
// on the paths no fallback/fanout coordinator touches, so a role's `api:`,
// `effort:` and `reasoning:` block mean here exactly what they mean there. The
// *serialization* is not shared — the provider's builders are unexported
// methods on its own plugin state (auth mode, multimodal config, prompt
// registry, Files preflight), and nexus.llm.batch is a separate plugin that may
// run with nexus.llm.openai absent entirely. Reaching into that package would
// be a plugin-to-plugin call; making it reachable would mean lifting OpenAI
// wire knowledge out of the provider into pkg/, which is a larger refactor than
// this file is. So the duplication the package doc already records stays, and
// is now recorded per surface rather than only for chat.
//
// The half this plugin genuinely cannot see is the *plugin-level* configuration
// of nexus.llm.openai: a plugin's config map is its own, and there is no
// cross-plugin read. That is why `providers.openai.api` and
// `providers.openai.reasoning` exist here — the same bargain the credentials
// already strike (`providers.openai.api_key_env`), for the same reason: the
// coordinator stays independently activatable and the operator states the
// deployment's shape once per plugin.

// OpenAI API surfaces this coordinator can address, spelled exactly as
// nexus.llm.openai spells them so one `api:` word means one thing everywhere.
const (
	openaiAPIChatCompletions = "chat_completions"
	openaiAPIResponses       = "responses"
)

// Batch JSONL `url` values, one per surface. The OpenAI Batch API accepts both;
// a batch's `endpoint` and every line's `url` inside it must agree, which is
// what makes resolveOpenAIBatchSurface refuse a mixed submit rather than
// building a file the API would reject as a whole.
const (
	openaiChatEndpointPath      = "/v1/chat/completions"
	openaiResponsesEndpointPath = "/v1/responses"
)

// defaultOpenAIAPI is the surface a batched request gets when neither the role
// nor this plugin declares one.
//
// It is `responses`, matching nexus.llm.openai's own default, and the match is
// not decoration — it is the whole point of the story this file implements. The
// provider narrows its default to `chat_completions` for a deployment that
// declared a `base_url` or an Azure auth mode, because those endpoints may not
// implement `/responses` at all. Neither narrowing case can arise here: this
// coordinator has no `base_url` key and no Azure mode, and always talks to
// `api.openai.com`, which is exactly the deployment the provider's unnarrowed
// default is chosen for. So the two agree by construction rather than by
// coincidence.
//
// This moves the endpoint for a deployment that merely upgrades. The opt-out is
// one key — `providers.openai.api: chat_completions` — and it is documented as
// a behaviour change in the reference.
const defaultOpenAIAPI = openaiAPIResponses

// openaiAPISurfaces is the accepted vocabulary, for error messages.
var openaiAPISurfaces = []string{openaiAPIChatCompletions, openaiAPIResponses}

func validOpenAIAPI(v string) bool {
	return v == openaiAPIChatCompletions || v == openaiAPIResponses
}

// openaiEndpointPath maps a surface onto the path that goes in both a line's
// `url` and the batch's `endpoint`.
func openaiEndpointPath(surface string) string {
	if surface == openaiAPIResponses {
		return openaiResponsesEndpointPath
	}
	return openaiChatEndpointPath
}

// effectiveOpenAIAPI is this coordinator's own surface: `providers.openai.api`
// when set, the default otherwise. A `core.models` entry's `api:` outranks it
// per request — see resolveOpenAIRequest.
func (p *Plugin) effectiveOpenAIAPI() string {
	if p.openaiAPI != "" {
		return p.openaiAPI
	}
	return defaultOpenAIAPI
}

// logger is a nil-safe accessor: the per-provider helpers are exercised
// directly by tests that build a Plugin without going through Init.
func (p *Plugin) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// resolveOpenAIRequest fills in every per-entry axis a batched request did not
// arrive carrying, and returns the surface it is addressed to alongside the
// completed request.
//
// engine.ResolveModelConfig is the same recovery the provider plugins make: the
// role the request names supplies `api:`, `effort:` and the native `reasoning:`
// block, the default role supplies only the shared axes, and anything already
// on the request wins outright. A nil registry — an embedder running without
// one — resolves to the request unchanged, which is why this is safe to call
// unconditionally.
//
// Precedence for the surface, most specific first:
//
//  1. the `api:` of the `core.models` entry the request resolved to;
//  2. this plugin's `providers.openai.api`;
//  3. defaultOpenAIAPI.
//
// An unrecognised per-entry `api:` is warned about and dropped to (2), the same
// degradation nexus.llm.openai's resolveAPI makes — a word neither plugin can
// honour must not become a URL.
func (p *Plugin) resolveOpenAIRequest(req events.LLMRequest) (string, events.LLMRequest) {
	resolved := engine.ResolveModelConfig(p.models, req)

	surface := p.effectiveOpenAIAPI()
	if v := resolved.Overrides.API; v != "" {
		if validOpenAIAPI(v) {
			surface = v
		} else {
			p.log().Warn("batch: ignoring an unrecognised core.models api on an openai entry; using the coordinator's",
				"role", resolved.Role,
				"api", v,
				"using", surface,
				"accepted", strings.Join(openaiAPISurfaces, ", "),
			)
		}
	}
	return surface, resolved
}

// resolveOpenAIBatchSurface resolves every request in one submit and returns the
// single surface they share, along with the resolved requests in the same order.
//
// One submit is one OpenAI batch, and an OpenAI batch carries exactly one
// `endpoint` that every line's `url` must match. So a submit whose roles
// disagree about `api:` is not a thing the API can express, and it fails here —
// naming both surfaces and both roles — rather than being silently flattened
// onto whichever surface the first request happened to want.
func (p *Plugin) resolveOpenAIBatchSurface(requests []events.BatchRequest) (string, []events.LLMRequest, error) {
	resolved := make([]events.LLMRequest, 0, len(requests))
	surface := ""
	surfaceFrom := ""
	for _, r := range requests {
		s, rr := p.resolveOpenAIRequest(r.Request)
		if surface == "" {
			surface, surfaceFrom = s, r.CustomID
		} else if s != surface {
			return "", nil, fmt.Errorf(
				"batch: openai submit mixes api surfaces — request %q resolves to %q and request %q to %q; one batch carries one endpoint, so split the submit",
				surfaceFrom, surface, r.CustomID, s)
		}
		resolved = append(resolved, rr)
	}
	return surface, resolved, nil
}

// --- reasoning --------------------------------------------------------------

// OpenAI reasoning modes, spelled as nexus.llm.openai spells them. `off` sends
// no reasoning configuration at all; `effort` declares the target a reasoning
// model.
const (
	openaiReasoningModeOff    = "off"
	openaiReasoningModeEffort = "effort"
)

// openaiReasoningEfforts is OpenAI's effort vocabulary, ascending. Deliberately
// unclamped and identical to the provider's list: it is a superset of the union
// of what nexus.llm.anthropic and nexus.llm.gemini accept, so every word an
// operator can write for either passes through verbatim.
var openaiReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// openaiReasoningSummaries is the accepted set for the summary verbosity dial.
var openaiReasoningSummaries = []string{"auto", "concise", "detailed"}

func validOpenAIEffort(v string) bool {
	for _, e := range openaiReasoningEfforts {
		if e == v {
			return true
		}
	}
	return false
}

func validOpenAISummary(v string) bool {
	for _, s := range openaiReasoningSummaries {
		if s == v {
			return true
		}
	}
	return false
}

// openaiReasoning is the resolved reasoning configuration for one batched
// request.
type openaiReasoning struct {
	Mode    string
	Effort  string
	Summary string

	// effortFromRole records that the resolved block itself named the effort,
	// which is what makes it outrank the role's shared `effort:`. Same rule as
	// the provider's effortSource, for the same reason: the parser sees a
	// merged block and cannot tell which half named the key.
	effortFromRole bool
}

// on reports whether this configuration puts anything on the wire.
func (r openaiReasoning) on() bool {
	return r.Mode == openaiReasoningModeEffort
}

// parseOpenAIReasoning validates one `reasoning:` block — the coordinator's own
// or a `core.models` entry's, merged.
//
// A nil block is mode off. A present block with no `mode` is mode effort, which
// is how a role turns reasoning on by naming only a depth. The deprecated
// `enabled` / `budget_tokens` spellings nexus.llm.openai still accepts are NOT
// accepted here: this key is new, so there is no deployment carrying them.
func parseOpenAIReasoning(block map[string]any) (openaiReasoning, error) {
	if block == nil {
		return openaiReasoning{Mode: openaiReasoningModeOff}, nil
	}
	rc := openaiReasoning{Mode: openaiReasoningModeEffort}

	if v, ok := block["mode"].(string); ok {
		switch v {
		case openaiReasoningModeEffort, openaiReasoningModeOff:
			rc.Mode = v
		default:
			return openaiReasoning{}, fmt.Errorf("reasoning.mode %q is not one of %s, %s",
				v, openaiReasoningModeEffort, openaiReasoningModeOff)
		}
	}
	if v, ok := block["effort"].(string); ok {
		if !validOpenAIEffort(v) {
			return openaiReasoning{}, fmt.Errorf("reasoning.effort %q is not one of %s",
				v, strings.Join(openaiReasoningEfforts, ", "))
		}
		rc.Effort = v
	}
	if v, ok := block["summary"].(string); ok {
		if !validOpenAISummary(v) {
			return openaiReasoning{}, fmt.Errorf("reasoning.summary %q is not one of %s",
				v, strings.Join(openaiReasoningSummaries, ", "))
		}
		rc.Summary = v
	}
	return rc, nil
}

// mergeOpenAIReasoningBlocks merges a `core.models` entry's block over the
// coordinator's, key by key: the entry wins on every key it mentions and a
// coordinator key it is silent about survives, so a role wanting only a deeper
// `effort` does not restate the mode. Neither input is mutated.
func mergeOpenAIReasoningBlocks(plugin, role map[string]any) map[string]any {
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

// openaiEffortFor picks the depth, specificity first: an `effort` the resolved
// block itself named, then the role's shared `effort:`, then the coordinator's
// own. An unrecognised shared `effort:` is dropped rather than sent — it is
// very likely a word aimed at another provider's entry in the same chain.
func openaiEffortFor(rc openaiReasoning, effort string) string {
	if rc.effortFromRole && rc.Effort != "" {
		return rc.Effort
	}
	if effort != "" && validOpenAIEffort(effort) {
		return effort
	}
	return rc.Effort
}

// resolveOpenAIReasoning picks the reasoning configuration for one already-
// resolved request.
//
// Precedence mirrors nexus.llm.openai's resolveReasoning exactly: the entry's
// own `reasoning:` block merged OVER the coordinator's (not substituted for
// it), then the role's `effort:` where the merged block leaves room, then the
// coordinator's block unchanged.
//
// The `mode` is the declaration throughout. A role carrying only an `effort:`
// on a coordinator whose own block is absent sends nothing: a bare depth cannot
// imply the target is a reasoning model, `reasoning` is an HTTP 400 on a model
// that is not one, and `effort` is a shared axis a role may be carrying for a
// different provider's entry in the same chain.
//
// An invalid entry block degrades to the coordinator's configuration with a
// warning rather than failing the submit — the coordinator's own block has
// already been validated at Init, so what is left is a hand-built request or a
// role block aimed at another provider.
func (p *Plugin) resolveOpenAIReasoning(req events.LLMRequest) openaiReasoning {
	rc := p.openaiReasoning
	if req.Overrides.Reasoning != nil {
		merged, err := parseOpenAIReasoning(mergeOpenAIReasoningBlocks(p.openaiReasoningRaw, req.Overrides.Reasoning))
		if err != nil {
			p.log().Warn("batch: ignoring an invalid per-entry openai reasoning block; using the coordinator's configuration",
				"role", req.Role,
				"error", err,
			)
		} else {
			if _, named := req.Overrides.Reasoning["effort"]; named && merged.Effort != "" {
				merged.effortFromRole = true
			}
			rc = merged
		}
	}
	rc.Effort = openaiEffortFor(rc, req.Effort)
	return rc
}

// openaiReasoningRejectedFields are the sampling parameters a reasoning model
// rejects. The list is the provider's, and it is one list because it describes
// a property of the model rather than of the endpoint — only some of them exist
// on any given surface.
var openaiReasoningRejectedFields = []string{
	"temperature", "top_p", "presence_penalty", "frequency_penalty",
	"logprobs", "top_logprobs", "prediction",
}

// applyOpenAIResponsesReasoning writes the Responses-shaped `reasoning` object
// onto a body, after stripping what a reasoning model rejects.
//
// Mode off — and an absent block, which resolves to it — writes nothing and
// strips nothing. Mode effort with neither a depth nor a summary resolved also
// writes nothing: an empty object says nothing the model's own defaults do not
// already say.
//
// There is deliberately no chat counterpart. The Chat Completions batch path is
// unchanged by this story, and on that surface a `reasoning_effort` alongside
// the tools every Nexus turn carries is refused from GPT-5.4 onward — so
// writing one there would trade a silent loss for a loud rejection rather than
// fixing anything. A deployment that wants depth in a batch declares
// `api: responses`, which is now also what it gets by default.
func applyOpenAIResponsesReasoning(body map[string]any, rc openaiReasoning) {
	if !rc.on() {
		return
	}
	for _, f := range openaiReasoningRejectedFields {
		delete(body, f)
	}
	reasoning := map[string]any{}
	if rc.Effort != "" {
		reasoning["effort"] = rc.Effort
	}
	if rc.Summary != "" {
		reasoning["summary"] = rc.Summary
	}
	if len(reasoning) > 0 {
		body["reasoning"] = reasoning
	}
}
