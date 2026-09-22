// Package gemini implements the Google Gemini LLM provider for Nexus.
//
// Supports both the public Generative Language API (api-key auth) and Vertex
// AI (service-account JWT auth). Feature parity with the OpenAI and Anthropic
// providers (sync + streaming, tool use, structured output, retry, cancel,
// debug logs, fallback hooks) plus Gemini-specific features: thinking parts
// (2.5 models), multimodal inputs (inline + Files API), code execution, and
// prompt caching via the cachedContents API.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.llm.gemini"
	pluginName = "Google Gemini LLM Provider"
	version    = "0.1.0"

	// defaultMaxTokens is the floor max_tokens applied when neither the
	// request, the request's role, nor the default role specifies one.
	defaultMaxTokens = 4096
)

// Plugin implements the Gemini LLM provider.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	models  *engine.ModelRegistry
	session *engine.SessionWorkspace
	replay  *engine.ReplayState
	// liveCalls counts API calls that survived the replay short-circuit.
	liveCalls atomic.Uint64

	auth    *authState
	client  *http.Client
	prompts *engine.PromptRegistry
	unsubs  []func()
	debug   bool
	retry   retryConfig
	// retryRaw is the plugin-level `retry:` block exactly as configured, kept
	// because a core.models role's block merges over it key by key and the
	// merged result has to go back through parseRetryConfig. nil means the
	// plugin set no block at all. Read-only — see mergeRetryBlock.
	retryRaw map[string]any
	pricing  *pricing.Table

	thinking thinkingConfig
	// thinkingRaw is the plugin-level `thinking:` block exactly as configured,
	// kept because a core.models role's block merges over it key by key and the
	// merged result has to go back through parseThinkingConfig. nil means the
	// plugin set no block at all.
	thinkingRaw   map[string]any
	codeExecution bool
	cache         *cacheState
	// cacheRaw is the plugin-level `cache:` block exactly as configured, kept
	// because a core.models role's block merges over it key by key and the
	// merged result has to go back through parseCacheSettings. nil means the
	// plugin set no block at all.
	cacheRaw map[string]any

	mu sync.Mutex
	// cancels indexes in-flight HTTP request cancel functions by
	// LLMRequest.RequestID; see matching field on Anthropic/OpenAI providers
	// for the concurrent-fan-out rationale.
	cancels    map[string]context.CancelFunc
	requestSeq int
}

// New creates a new Gemini provider plugin.
func New() engine.Plugin {
	return &Plugin{}
}

// LiveCalls returns the number of llm.request handler invocations that
// passed the replay short-circuit.
func (p *Plugin) LiveCalls() uint64 {
	return p.liveCalls.Load()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.models = ctx.Models
	p.session = ctx.Session
	p.prompts = ctx.Prompts
	p.replay = ctx.Replay

	if debug, ok := ctx.Config["debug"].(bool); ok {
		p.debug = debug
	}

	auth, err := resolveAuth(ctx.Config)
	if err != nil {
		return err
	}
	p.auth = auth

	// Vertex only: mint one token now so a broken pod identity fails the boot
	// instead of the first user message, and log the single INFO line that
	// records which credential source answered. See confirmCredentials in
	// auth.go, including the logging-rubric exception documented there.
	if err := p.auth.confirmCredentials(context.Background(), p.log()); err != nil {
		return err
	}

	p.client = &http.Client{Timeout: 5 * time.Minute}

	p.pricing = parsePricingConfig(ctx.Config)
	retry, err := parseRetryConfig(ctx.Config, p.log())
	if err != nil {
		return err
	}
	p.retry = retry
	p.retryRaw = rawRetryBlock(ctx.Config)

	// A core.models role may carry a `retry:` block of its own, which merges
	// over the plugin-level one. Like the `thinking:` and `cache:` blocks
	// below, these never pass through schema.json, so this sweep is the only
	// thing that checks them — at boot, rather than at the first request that
	// names the role.
	if err := validateRoleRetry(p.models, p.retryRaw, p.log()); err != nil {
		return err
	}

	thinking, err := parseThinkingConfig(ctx.Config, p.log())
	if err != nil {
		return err
	}
	p.thinking = thinking
	p.thinkingRaw = rawThinkingBlock(ctx.Config)

	// A role's `thinking:` block and its `effort:` are both configuration facts,
	// so both are checked once here rather than per request: an invalid merged
	// block fails the boot naming the role, and the clamp of Anthropic's
	// xhigh/max onto Gemini's high is warned once per (role, value).
	if err := validateRoleThinking(p.models, p.thinkingRaw, p.thinking, p.log()); err != nil {
		return err
	}

	if v, ok := ctx.Config["code_execution"].(bool); ok {
		p.codeExecution = v
	}

	cache, err := newCacheState(ctx.Config, p.logger)
	if err != nil {
		return err
	}
	p.cache = cache
	p.cacheRaw = rawCacheBlock(ctx.Config)

	// A core.models role may carry a `cache:` block of its own, which merges
	// over the plugin-level one. Like the `thinking:` blocks above, these never
	// pass through schema.json, so this sweep is the only thing that checks
	// them — at boot, rather than at the first request that names the role.
	if err := validateRoleCache(p.models, p.cacheRaw, p.log()); err != nil {
		return err
	}

	if p.retry.Enabled {
		p.logger.Debug("retry enabled",
			"max_retries", p.retry.MaxRetries,
			"backoff", string(p.retry.Backoff),
			"initial_delay", p.retry.InitialDelay,
			"max_delay", p.retry.MaxDelay,
		)
	}
	if p.thinking.Mode != thinkingModeOff {
		p.logger.Debug("thinking enabled",
			"mode", string(p.thinking.Mode),
			"level", p.thinking.Level,
			"budget_tokens", p.thinking.BudgetTokens,
			"include_thoughts", p.thinking.IncludeThoughts,
		)
	}
	if p.codeExecution {
		p.logger.Debug("code_execution tool enabled")
	}
	if p.cache.enabled {
		p.logger.Debug("prompt caching enabled",
			"min_tokens", p.cache.minTokens,
			"ttl", p.cache.ttl,
		)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("llm.request", p.handleEvent,
			engine.WithPriority(10),
			engine.WithSource(pluginID),
		),
		p.bus.Subscribe("cancel.active", p.handleCancel,
			engine.WithPriority(5),
			engine.WithSource(pluginID),
		),
	)

	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.client.CloseIdleConnections()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "llm.request", Priority: 10},
		{EventType: "cancel.active", Priority: 5},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:llm.response",
		"llm.response",
		"llm.stream.chunk",
		"before:llm.stream.chunk",
		"llm.stream.hold",
		"llm.stream.retract",
		"llm.stream.end",
		"thinking.step",
		"tool.invoke",
		"tool.result",
		"before:core.error",
		"core.error",
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	if event.Type != "llm.request" {
		return
	}
	req, ok := event.Payload.(events.LLMRequest)
	if !ok {
		p.emitError(fmt.Errorf("gemini: invalid llm.request payload type: %T", event.Payload))
		return
	}
	p.handleRequest(req)
}

func (p *Plugin) handleCancel(_ engine.Event[any]) {
	p.mu.Lock()
	cancels := p.cancels
	p.cancels = make(map[string]context.CancelFunc)
	p.mu.Unlock()

	if len(cancels) == 0 {
		return
	}
	p.logger.Info("cancelling in-flight LLM requests", "count", len(cancels))
	for _, cancel := range cancels {
		cancel()
	}
}

// resolvedTarget is what one llm.request resolves to before the body is built:
// the concrete model, the max_tokens and the reasoning-depth hint the call will
// carry.
type resolvedTarget struct {
	model     string
	maxTokens int

	// effort is the core.models reasoning-depth hint. Core does no validation
	// of it by design — the vocabularies differ per provider — so this
	// provider owns its own, in thinking.go.
	effort string

	// skip is set when the resolved role names some other provider, in which
	// case this plugin must not answer the request at all.
	skip bool
}

// resolveTarget turns a request's Model/Role into the concrete model,
// max_tokens and effort the API call will carry. It mirrors the Anthropic
// provider's method of the same name, including its effort precedence.
func (p *Plugin) resolveTarget(req events.LLMRequest) resolvedTarget {
	t := resolvedTarget{
		model:     req.Model,
		maxTokens: req.MaxTokens,

		// An effort already on the request wins over anything the registry
		// says. The fallback and fanout coordinators copy the chain entry they
		// are actually serving onto the outgoing request, and only that field
		// is correct for a fallback entry or a non-first fanout entry — a
		// Resolve() here always reads chain[0]. The branches below therefore
		// only fill in a value the request arrived without, which is the
		// ordinary single-entry role: nothing stamps req.Effort for one, so
		// without them a role's `effort:` would never reach the wire.
		effort: req.Effort,
	}

	// Resolve model role if no explicit model is set.
	if t.model == "" && p.models != nil {
		if cfg, ok := p.models.Resolve(req.Role); ok {
			// If the resolved config targets a different provider, skip this request.
			if cfg.Provider != "" && cfg.Provider != pluginID {
				t.skip = true
				return t
			}
			if cfg.Model != "" {
				t.model = cfg.Model
			}
			if t.maxTokens == 0 && cfg.MaxTokens > 0 {
				t.maxTokens = cfg.MaxTokens
			}
			if t.effort == "" {
				t.effort = cfg.Effort
			}
		}
	}

	// Fall back to default model role.
	if t.model == "" && p.models != nil {
		def := p.models.Default()
		if def.Provider == "" || def.Provider == pluginID {
			t.model = def.Model
			if t.maxTokens == 0 {
				t.maxTokens = def.MaxTokens
			}
			if t.effort == "" && def.Effort != "" {
				t.effort = def.Effort
			}
		}
	}

	// max_tokens may still be 0 — common when a router (idea 09) rewrote
	// req.Model to a concrete id without touching MaxTokens, so the
	// model-resolution branches above were skipped.
	//
	// Effort needs the same recovery for the same reason: a rewritten
	// req.Model skips the branches above, and an effort dropped there would be
	// dropped silently. It has no equivalent of defaultMaxTokens — an unset
	// effort is a valid state that emits no thinkingLevel at all, leaving the
	// model's own default level to stand.
	if t.maxTokens == 0 && p.models != nil && req.Role != "" {
		if cfg, ok := p.models.Resolve(req.Role); ok && cfg.MaxTokens > 0 {
			t.maxTokens = cfg.MaxTokens
		}
	}
	if t.effort == "" && p.models != nil && req.Role != "" {
		if cfg, ok := p.models.Resolve(req.Role); ok && cfg.Effort != "" {
			t.effort = cfg.Effort
		}
	}
	if t.maxTokens == 0 && p.models != nil {
		if def := p.models.Default(); def.MaxTokens > 0 {
			t.maxTokens = def.MaxTokens
		}
	}
	if t.effort == "" && p.models != nil {
		if def := p.models.Default(); def.Effort != "" {
			t.effort = def.Effort
		}
	}
	if t.maxTokens == 0 {
		t.maxTokens = defaultMaxTokens
	}

	return t
}

// applyEntryOverrides copies the per-entry axes this provider actually consumes
// from the `core.models` chain entry being served onto the request handleRequest
// works with. It mirrors the Anthropic provider's method of the same name, and
// is the counterpart of resolveTarget for the axes that need no
// provider-specific resolution of their own.
//
// A fallback or fanout coordinator has already stamped the entry when one is
// involved, and engine.ResolveModelConfig leaves a stamped request alone;
// otherwise it recovers the entry from the registry, covering the named role,
// the default role and the late case where a router rewrote `model` and left
// `role` alone.
//
// Deliberately a narrow slice rather than `*req = engine.ResolveModelConfig(...)`:
// model, max_tokens and effort already have their own resolution pass in
// resolveTarget — one that knows about defaultMaxTokens, this provider's effort
// vocabulary and the foreign-provider skip — so taking them wholesale here would
// duplicate and quietly change it. The remaining axes (`reasoning`, `api`) have
// no consumer in this provider.
//
// Temperature is a plain gap-fill, and the precedence is the engine-wide one:
// ResolveModelConfig never disturbs a value the request already carries, so an
// agent posture's temperature — and the approval_policy gate's — still beats a
// `core.models` role's. A role's value only fills the gap when nothing upstream
// set one. `0` is a real value, so the axis is a *float64 the whole way down.
//
// `cache` behaves like `thinking`: the recovered block is merged over the
// plugin-level one rather than substituted for it, by resolveCache at the one
// point that reads it — the cachedContent lookup in the body builder.
//
// `retry` is the odd one out: it is merged the same way, by resolveRetry, but
// it reaches no part of the request body at all. Its one consumer is the loop
// in doWithRetry, which is why that function takes a retryConfig rather than
// reading the plugin's.
func (p *Plugin) applyEntryOverrides(req *events.LLMRequest) {
	resolved := engine.ResolveModelConfig(p.models, *req)
	req.Overrides.Thinking = resolved.Overrides.Thinking
	req.Overrides.Cache = resolved.Overrides.Cache
	req.Overrides.Retry = resolved.Overrides.Retry
	req.Temperature = resolved.Temperature
}

func (p *Plugin) handleRequest(req events.LLMRequest) {
	if target, ok := req.Metadata["_target_provider"].(string); ok && target != pluginID {
		return
	}

	// Replay publishes journaled responses directly rather than through
	// engine.PublishLLMResponse: these are re-published history, not fresh
	// model output, and the recorded response is already the post-gate one
	// subscribers saw on the original run. Re-running before:llm.response
	// here would re-gate an already-gated response and break replay fidelity.
	if p.replay != nil && p.replay.Active() {
		raw, ok := p.replay.Pop("llm.response")
		if !ok {
			p.logger.Warn("gemini: replay stash empty for llm.request — emitting empty response")
			_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: req.Model})
			return
		}
		resp, err := journal.PayloadAs[events.LLMResponse](raw)
		if err != nil {
			p.logger.Warn("gemini: replay payload decode failed", "error", err)
			_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: req.Model})
			return
		}
		_ = p.bus.Emit("llm.response", resp)
		return
	}
	p.liveCalls.Add(1)

	target := p.resolveTarget(req)
	if target.skip {
		return
	}
	model := target.model
	maxTokens := target.maxTokens
	effort := target.effort

	if model == "" {
		p.emitError(fmt.Errorf("gemini: no model resolved for role %q", req.Role))
		return
	}

	// The serving chain entry's `thinking:` and `cache:` blocks and its
	// `temperature:`. req is a value parameter, so the caller's payload is
	// untouched.
	p.applyEntryOverrides(&req)

	p.logger.Log(context.Background(), engine.LevelTrace, "resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens, "effort", effort)

	body, err := p.buildRequestBody(model, maxTokens, effort, req)
	if err != nil {
		p.emitError(fmt.Errorf("gemini: build request: %w", err))
		return
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		p.emitError(fmt.Errorf("gemini: marshal request: %w", err))
		return
	}

	if p.debug {
		p.mu.Lock()
		p.requestSeq++
		p.mu.Unlock()
		p.debugLog("request", jsonBody)
	}

	op := "generateContent"
	if req.Stream {
		op = "streamGenerateContent"
	}
	url := p.auth.apiURL(model, op)

	reqCtx, cancel := context.WithCancel(context.Background())
	if req.RequestID == "" {
		req.RequestID = engine.GenerateID()
	}
	p.mu.Lock()
	if p.cancels == nil {
		p.cancels = make(map[string]context.CancelFunc)
	}
	p.cancels[req.RequestID] = cancel
	p.mu.Unlock()

	makeReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if err := p.auth.applyAuth(reqCtx, httpReq); err != nil {
			return nil, fmt.Errorf("apply auth: %w", err)
		}
		return httpReq, nil
	}

	// The serving chain entry's `retry:` block, merged over the plugin's. Retry
	// is call-time behaviour rather than a request field, so this is the only
	// place a role's block can take effect.
	resp, err := p.doWithRetry(reqCtx, p.resolveRetry(req), makeReq)
	if err != nil {
		p.mu.Lock()
		delete(p.cancels, req.RequestID)
		p.mu.Unlock()
		if reqCtx.Err() == context.Canceled {
			p.logger.Info("LLM request cancelled")
			return
		}
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("gemini: HTTP request failed: %w", err),
			Retryable:        true,
			RetriesExhausted: true,
			RequestMeta:      req.Metadata,
			RequestID:        req.RequestID,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("gemini: API returned status %d: %s", resp.StatusCode, string(respBody)),
			Retryable:   false,
			RequestMeta: req.Metadata,
			RequestID:   req.RequestID,
		})
		return
	}

	// Build per-request meta locally so concurrent in-flight requests cannot
	// clobber each other's tags/meta via shared plugin slots.
	meta := req.Metadata
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "text" {
		if meta == nil {
			meta = make(map[string]any)
		}
		meta["_structured_output"] = true
	}

	var responseBody io.Reader = resp.Body
	var debugBuf *bytes.Buffer
	if p.debug {
		debugBuf = new(bytes.Buffer)
		responseBody = io.TeeReader(resp.Body, debugBuf)
	}

	if req.Stream {
		p.handleStreamResponse(responseBody, req.RequestID, meta, req.Tags)
	} else {
		p.handleSyncResponse(responseBody, req.RequestID, meta, req.Tags)
	}

	if debugBuf != nil {
		p.debugLog("response", debugBuf.Bytes())
	}

	p.mu.Lock()
	delete(p.cancels, req.RequestID)
	p.mu.Unlock()
}

// buildRequestBody constructs a Gemini generateContent request body.
//
// effort is the reasoning-depth hint the caller resolved for this request —
// events.LLMRequest.Effort when something stamped it, otherwise the
// core.models role's `effort:`. It is not read off req, because req.Effort
// alone is only ever set by the fallback and fanout coordinators.
func (p *Plugin) buildRequestBody(model string, maxTokens int, effort string, req events.LLMRequest) (map[string]any, error) {
	body := map[string]any{}

	// generationConfig.
	gen := map[string]any{}
	if maxTokens > 0 {
		gen["maxOutputTokens"] = maxTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}

	// Structured output: native response_schema.
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			gen["responseMimeType"] = "application/json"
		case "json_schema":
			gen["responseMimeType"] = "application/json"
			if rf.Schema != nil {
				gen["responseSchema"] = sanitizeSchemaForGemini(rf.Schema)
			}
		}
	}

	// Thinking config: exactly one of thinkingLevel / thinkingBudget, or
	// nothing at all under mode: off — whichever layer supplied the value.
	// resolveThinking merges the serving role's own `thinking:` block over the
	// plugin-level one; effort is the role's reasoning-depth hint, read only
	// under mode: level and only where that block named no `level` itself.
	if err := applyThinking(gen, p.resolveThinking(req), effort); err != nil {
		return nil, err
	}

	if len(gen) > 0 {
		body["generationConfig"] = gen
	}

	// Pre-upload any oversize multimodal parts via the Files API so the rest of
	// the request can stay in a single HTTP call.
	preparedMsgs, err := p.preuploadParts(context.Background(), req.Messages)
	if err != nil {
		return nil, err
	}

	// System instruction + contents.
	systemPrompt, contents, err := p.convertMessages(preparedMsgs)
	if err != nil {
		return nil, err
	}

	if p.prompts != nil {
		systemPrompt = p.prompts.Apply(systemPrompt)
	}
	if systemPrompt != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": systemPrompt}},
		}
	}
	body["contents"] = contents

	// Tool filtering.
	filteredTools := applyToolFilter(req.Tools, req.ToolFilter)
	if req.ToolChoice != nil && req.ToolChoice.Mode == "none" {
		// Gemini's NONE mode keeps tools listed but disabled. To match the
		// other providers' "strip everything" behavior we drop them entirely.
		filteredTools = nil
	}

	var toolGroups []map[string]any
	if len(filteredTools) > 0 {
		decls := make([]map[string]any, 0, len(filteredTools))
		for _, t := range filteredTools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  sanitizeSchemaForGemini(t.Parameters),
			})
		}
		toolGroups = append(toolGroups, map[string]any{
			"functionDeclarations": decls,
		})
	}
	if p.codeExecution {
		toolGroups = append(toolGroups, map[string]any{"codeExecution": map[string]any{}})
	}
	if len(toolGroups) > 0 {
		body["tools"] = toolGroups
	}

	if tc := resolveToolChoice(req.ToolChoice, filteredTools); tc != nil {
		body["toolConfig"] = tc
	}

	// Prompt caching: replace the stable prefix with cached_content reference
	// when eligible. resolveCache merges the serving role's own `cache:` block
	// over the plugin-level one, so a role can opt out of (or into) the shared
	// entry map without changing the deployment default.
	if cc := p.resolveCache(req); cc.enabled {
		if cachedName := p.cache.lookupWith(true, model, systemPrompt, filteredTools, contents); cachedName != "" {
			body["cachedContent"] = cachedName
			// Caller's contents already includes only the trailing delta; the
			// cache plugin stripped what's covered by the cache. See cache.go.
		}
	}

	return body, nil
}

// log returns the plugin logger, falling back to slog.Default() when the
// plugin was built without one. convertMessages is reachable on a zero-value
// Plugin (it needs no bus or config), so a warning raised from there must not
// depend on Init having run.
func (p *Plugin) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// convertMessages walks the message list, extracts the system prompt, and
// converts the rest into Gemini "contents" entries. Tool results are folded
// into user-role entries with functionResponse parts.
func (p *Plugin) convertMessages(msgs []events.Message) (string, []map[string]any, error) {
	var systemPrompt strings.Builder
	var out []map[string]any

	// Maintain a name→toolCallID lookup so we can correlate tool results back
	// to the function name (Gemini's functionResponse needs a name, not an ID).
	toolCallNames := make(map[string]string)

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if systemPrompt.Len() > 0 {
				systemPrompt.WriteString("\n\n")
			}
			systemPrompt.WriteString(msg.Content)

		case "user":
			parts, err := buildParts(msg)
			if err != nil {
				return "", nil, err
			}
			out = append(out, map[string]any{
				"role":  "user",
				"parts": parts,
			})

		case "assistant":
			var parts []map[string]any
			if msg.Content != "" {
				parts = append(parts, map[string]any{"text": msg.Content})
			}
			// Signatures captured when this turn was first received, carried
			// here on the stored message by the memory plugins. Sparse by
			// design — see storedThoughtSignatures.
			sigs := storedThoughtSignatures(msg.Metadata)
			firstCall := true
			for _, tc := range msg.ToolCalls {
				var args any
				if tc.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
						args = map[string]any{}
					}
				} else {
					args = map[string]any{}
				}
				part := map[string]any{
					"functionCall": map[string]any{
						"name": tc.Name,
						"args": args,
					},
				}
				// thoughtSignature is a sibling of functionCall on the Part,
				// not a field inside it. Omit the key entirely when absent:
				// an empty string is a distinct, invalid wire value.
				if sig := sigs[tc.ID]; sig != "" {
					part["thoughtSignature"] = sig
				} else if firstCall {
					// Only the FIRST functionCall part of a model turn is
					// required to carry a signature; in a parallel batch
					// Gemini issues just one, bound to that first call, so
					// parts 2..N are legitimately bare and warning on them
					// would be noise on correct behavior.
					//
					// Deliberately non-defensive: the part goes out bare and
					// the API decides. Dropping the call would orphan its
					// paired functionResponse (a 400 of its own), and
					// synthesizing a placeholder signature would rewrite
					// history with a value the model never issued.
					p.log().Warn("gemini: replaying functionCall without a thoughtSignature; the API may reject this request with a 400",
						"call_id", tc.ID, "tool", tc.Name)
				}
				firstCall = false
				parts = append(parts, part)
				toolCallNames[tc.ID] = tc.Name
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, map[string]any{
				"role":  "model",
				"parts": parts,
			})

		case "tool":
			name := toolCallNames[msg.ToolCallID]
			if name == "" {
				// Best effort: use ToolCallID as the function name. Engine
				// callers may emit synthetic IDs equal to the tool name.
				name = msg.ToolCallID
			}
			// Gemini expects functionResponse.response to be a structured object
			// (a protobuf Struct). Wrap raw text in {output: ...} for round-trip
			// safety, and likewise wrap any valid-JSON result that is not an
			// object — a top-level array or scalar (e.g. a tool returning a list
			// of matches) is otherwise rejected with "Proto field is not
			// repeating, cannot start list".
			var responseObj any
			if err := json.Unmarshal([]byte(msg.Content), &responseObj); err != nil {
				responseObj = map[string]any{"output": msg.Content}
			}
			if _, isObj := responseObj.(map[string]any); !isObj {
				responseObj = map[string]any{"output": responseObj}
			}
			out = append(out, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": map[string]any{
						"name":     name,
						"response": responseObj,
					},
				}},
			})

		default:
			// Unknown role: pass through as user text.
			out = append(out, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": msg.Content}},
			})
		}
	}

	return systemPrompt.String(), out, nil
}

// sanitizeSchemaForGemini strips JSON Schema keywords Gemini's schema dialect
// doesn't accept (e.g. additionalProperties, $schema, $id) and collapses
// draft 2020-12 "type" unions (e.g. ["string","null"], as commonly emitted by
// MCP servers built with zod-to-json-schema) into Gemini's singular "type"
// plus "nullable", since Gemini's Schema.type is a non-repeating enum and
// rejects an array outright. Walks recursively.
func sanitizeSchemaForGemini(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	nullable := false
	for k, v := range schema {
		switch k {
		case "$schema", "$id", "$ref", "additionalProperties", "definitions", "$defs":
			continue
		}
		if k == "type" {
			if types, ok := v.([]any); ok {
				resolved := ""
				for _, t := range types {
					ts, _ := t.(string)
					if ts == "null" {
						nullable = true
						continue
					}
					if ts != "" && resolved == "" {
						resolved = ts
					}
				}
				if resolved == "" {
					resolved = "string"
				}
				out["type"] = resolved
				continue
			}
		}
		switch vv := v.(type) {
		case map[string]any:
			out[k] = sanitizeSchemaForGemini(vv)
		case []any:
			arr := make([]any, len(vv))
			for i, item := range vv {
				if m, ok := item.(map[string]any); ok {
					arr[i] = sanitizeSchemaForGemini(m)
				} else {
					arr[i] = item
				}
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	// Gemini requires Schema.items on every array-typed schema and rejects
	// the request outright when it is absent ("...properties[x].items:
	// missing field."). JSON Schema treats items as optional, and MCP
	// servers routinely omit it for free-form arrays (e.g. an RFC 6902
	// patch ops array). An empty item schema satisfies the proto without
	// inventing an element type the tool never declared.
	if t, _ := out["type"].(string); strings.EqualFold(t, "array") {
		if _, ok := out["items"]; !ok {
			out["items"] = map[string]any{}
		}
	}
	if nullable {
		out["nullable"] = true
	}
	return out
}

// API response shapes.

type apiResponse struct {
	Candidates    []apiCandidate    `json:"candidates"`
	UsageMetadata *apiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
}

type apiCandidate struct {
	Content      apiContent `json:"content"`
	FinishReason string     `json:"finishReason"`
	Index        int        `json:"index"`
}

type apiContent struct {
	Parts []apiPart `json:"parts"`
	Role  string    `json:"role"`
}

type apiPart struct {
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"`
	// ThoughtSignature is Gemini's opaque, encrypted per-part reasoning token.
	// It is never interpreted here — see thoughtSignatures for why it has to be
	// captured and what it is later used for.
	ThoughtSignature    string             `json:"thoughtSignature,omitempty"`
	FunctionCall        *apiFunctionCall   `json:"functionCall,omitempty"`
	ExecutableCode      *apiExecutableCode `json:"executableCode,omitempty"`
	CodeExecutionResult *apiCodeExecResult `json:"codeExecutionResult,omitempty"`
	InlineData          *apiInlineData     `json:"inlineData,omitempty"`
	FileData            map[string]any     `json:"fileData,omitempty"`
}

type apiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type apiExecutableCode struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

type apiCodeExecResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

type apiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type apiUsageMetadata struct {
	PromptTokenCount        int                 `json:"promptTokenCount"`
	CandidatesTokenCount    int                 `json:"candidatesTokenCount"`
	TotalTokenCount         int                 `json:"totalTokenCount"`
	ThoughtsTokenCount      int                 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int                 `json:"cachedContentTokenCount"`
	PromptTokensDetails     []apiModalityDetail `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails []apiModalityDetail `json:"candidatesTokensDetails,omitempty"`
	CacheTokensDetails      []apiModalityDetail `json:"cacheTokensDetails,omitempty"`
}

// apiModalityDetail mirrors Gemini's `{ modality, tokenCount }` entries inside
// promptTokensDetails / candidatesTokensDetails. Modality is one of "TEXT",
// "IMAGE", "AUDIO", "VIDEO", "DOCUMENT" per the API.
type apiModalityDetail struct {
	Modality   string `json:"modality"`
	TokenCount int    `json:"tokenCount"`
}

// thoughtSignatureMetaKey is the events.LLMResponse.Metadata key under which
// captured Gemini thoughtSignature values are published. The value is a
// map[string]string keyed by the synthesized tool-call ID the signature arrived
// with, so the request-side re-emit path can look one up by
// events.ToolCallRequest.ID.
const thoughtSignatureMetaKey = "gemini_thought_signatures"

// thoughtSignatures accumulates the opaque thoughtSignature values Gemini
// attaches to the parts of a model turn.
//
// Gemini 3 — and, in field reports, 2.5 as well — rejects a follow-up request
// with a 400 ("Function call ... is missing a thought_signature") when a
// replayed model turn comes back without the signature it was issued with. The
// plugin therefore has to carry the value across the turn, and that starts with
// not throwing it away at decode time.
//
// Two properties of the wire format make this non-obvious:
//
//  1. Signatures are SPARSE BY DESIGN. In a parallel tool-call batch Gemini
//     attaches the signature only to the FIRST functionCall part; parts 2..N
//     carry none. A missing signature is therefore normal — it is never logged,
//     defaulted or treated as an error at capture time.
//
//  2. A signature can ride on a part that matches no arm of the part-type
//     switches below: under streaming the API may deliver it on a part with
//     empty text and no functionCall. Capture consequently runs ABOVE those
//     switches rather than as another case, which an earlier arm would shadow
//     for every part that also carries text or a call.
//
// The value itself is opaque and encrypted: it is stored and later echoed
// verbatim, never parsed, validated, truncated or reformatted.
type thoughtSignatures struct {
	byKey      map[string]string
	contentSeq int
}

// capture records part's signature, if it has one.
//
// toolCallSeq must be the counter value the part-type switch is about to use
// for this part, so a signature riding on a functionCall part is keyed by
// exactly the `call_<seq>_<name>` ID that part's events.ToolCallRequest gets.
// A signature on any other part is keyed under a reserved `_content_<n>` name
// that cannot collide with a synthesized call ID; echoing those back is
// optional per the API docs, but dropping them here would foreclose the choice.
func (s *thoughtSignatures) capture(part apiPart, toolCallSeq int) {
	if part.ThoughtSignature == "" {
		return
	}
	var key string
	if part.FunctionCall != nil {
		key = fmt.Sprintf("call_%d_%s", toolCallSeq, part.FunctionCall.Name)
	} else {
		key = fmt.Sprintf("_content_%d", s.contentSeq)
		s.contentSeq++
	}
	if s.byKey == nil {
		s.byKey = make(map[string]string)
	}
	s.byKey[key] = part.ThoughtSignature
}

// metadata returns the Metadata fragment to publish on the llm.response, or nil
// when the turn carried no signature at all. The key is omitted rather than set
// to an empty map so a reader can treat its presence as "this turn had
// signatures" without inspecting the value.
func (s *thoughtSignatures) metadata() map[string]any {
	if len(s.byKey) == 0 {
		return nil
	}
	return map[string]any{thoughtSignatureMetaKey: s.byKey}
}

// storedThoughtSignatures is the read side of thoughtSignatures.metadata(): it
// pulls the call-ID→signature map back off a stored events.Message so
// convertMessages can re-attach each signature to the functionCall part it was
// issued with. Returns nil when the message carries none, or when the value is
// any shape other than a string-valued map.
//
// The dual-shape switch is load-bearing, not defensive padding. The capture
// path publishes a map[string]string, but events.Message.Metadata is
// map[string]any and conversation history is persisted as JSONL: after a
// save/reload cycle encoding/json hands the same value back as a
// map[string]any with any-boxed strings. A single-shape assertion would work
// for the live turn and silently return nothing on every resumed session —
// exactly the case the 400 shows up in. Same reasoning, and same structure, as
// prependThinkingBlocks in the Anthropic provider.
//
// Keys carrying the reserved `_content_<n>` prefix are ignored here: those
// signatures rode on non-functionCall parts, and echoing them back is
// "recommended" but not enforced by the API, so this path deliberately emits
// only the functionCall ones. They are keyed by synthesized call ID
// (`call_<seq>_<name>`), which cannot collide with the reserved prefix, so a
// plain lookup by events.ToolCallRequest.ID never returns one.
func storedThoughtSignatures(meta map[string]any) map[string]string {
	if meta == nil {
		return nil
	}
	raw, ok := meta[thoughtSignatureMetaKey]
	if !ok {
		return nil
	}
	switch sigs := raw.(type) {
	case map[string]string:
		// In-memory shape, straight off the llm.response of the live turn.
		return sigs
	case map[string]any:
		// Post-JSON-roundtrip shape. Non-string values are skipped rather
		// than coerced — a signature is an opaque string or it is nothing.
		out := make(map[string]string, len(sigs))
		for k, v := range sigs {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
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

// mergeMetadata returns a new map holding every key of `into` plus every key of
// `from`, with `from` winning on collision. Either input may be nil; the result
// is nil only when both are empty.
//
// Allocating a fresh map is load-bearing: the request metadata threaded into
// the response paths aliases events.LLMRequest.Metadata, which the caller still
// owns, so provider-side keys must never be written into it directly.
func mergeMetadata(into, from map[string]any) map[string]any {
	if len(into) == 0 && len(from) == 0 {
		return nil
	}
	out := make(map[string]any, len(into)+len(from))
	for k, v := range into {
		out[k] = v
	}
	for k, v := range from {
		out[k] = v
	}
	return out
}

func (p *Plugin) handleSyncResponse(body io.Reader, requestID string, meta map[string]any, tags map[string]string) {
	var apiResp apiResponse
	if err := json.NewDecoder(body).Decode(&apiResp); err != nil {
		p.emitError(fmt.Errorf("gemini: decode response: %w", err))
		return
	}

	resp := p.convertAPIResponse(apiResp, generateTurnID())
	resp.RequestID = requestID
	// Request-passthrough metadata (e.g. _structured_output) merges onto the
	// metadata convertAPIResponse already attached (the captured signatures).
	resp.Metadata = mergeMetadata(resp.Metadata, meta)
	resp.Tags = tags

	engine.PublishLLMResponse(p.bus, resp)
}

// convertAPIResponse normalizes a Gemini response into events.LLMResponse.
// Thinking parts are emitted as thinking.step events (when turnID is non-empty)
// or skipped from Content. Code execution parts are dual-emitted as content
// and tool.invoke / tool.result events.
func (p *Plugin) convertAPIResponse(apiResp apiResponse, turnID string) events.LLMResponse {
	var content strings.Builder
	var toolCalls []events.ToolCallRequest
	var finishReason string
	var sigs thoughtSignatures
	model := apiResp.ModelVersion

	if len(apiResp.Candidates) > 0 {
		cand := apiResp.Candidates[0]
		finishReason = cand.FinishReason

		toolCallSeq := 0
		for _, part := range cand.Content.Parts {
			// Above the switch on purpose: a signature can arrive on a part
			// that matches none of the arms below. See thoughtSignatures.
			sigs.capture(part, toolCallSeq)

			switch {
			case part.Thought && part.Text != "":
				_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: turnID,
					Source:    pluginID,
					Content:   part.Text,
					Phase:     "reasoning",
					Timestamp: time.Now(),
				})

			case part.Text != "":
				content.WriteString(part.Text)

			case part.FunctionCall != nil:
				args, _ := json.Marshal(part.FunctionCall.Args)
				id := fmt.Sprintf("call_%d_%s", toolCallSeq, part.FunctionCall.Name)
				toolCalls = append(toolCalls, events.ToolCallRequest{
					ID:        id,
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				})
				toolCallSeq++

			case part.ExecutableCode != nil:
				code := part.ExecutableCode.Code
				lang := part.ExecutableCode.Language
				content.WriteString(fmt.Sprintf("\n```%s\n%s\n```\n", strings.ToLower(lang), code))
				_ = p.bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "_gemini_code_execution",
					Arguments: map[string]any{"language": lang, "code": code},
				})

			case part.CodeExecutionResult != nil:
				out := part.CodeExecutionResult.Output
				outcome := part.CodeExecutionResult.Outcome
				content.WriteString(fmt.Sprintf("\n```output\n%s\n```\n", out))
				toolResult := events.ToolResult{SchemaVersion: events.ToolResultVersion, Name: "_gemini_code_execution",
					Output: out,
				}
				if outcome != "OUTCOME_OK" {
					toolResult.Error = outcome
				}
				_ = p.bus.Emit("tool.result", toolResult)
			}
		}
	}

	usage := events.Usage{}
	if apiResp.UsageMetadata != nil {
		usage.PromptTokens = apiResp.UsageMetadata.PromptTokenCount
		usage.CompletionTokens = apiResp.UsageMetadata.CandidatesTokenCount
		usage.TotalTokens = apiResp.UsageMetadata.TotalTokenCount
		usage.ReasoningTokens = apiResp.UsageMetadata.ThoughtsTokenCount
		usage.CachedTokens = apiResp.UsageMetadata.CachedContentTokenCount
		if mb := geminiModalityBreakdown(apiResp.UsageMetadata); len(mb) > 0 {
			usage.ModalityBreakdown = mb
		}
	}

	return events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: content.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		CostUSD:      p.costForModel(model, usage),
		Model:        model,
		FinishReason: finishReason,
		Metadata:     sigs.metadata(),
	}
}

// generateTurnID returns a synthetic per-stream identifier. Gemini's API does
// not assign one and the engine relies on a stable TurnID to associate
// streaming chunks with the bubble they belong to.
func generateTurnID() string {
	return fmt.Sprintf("gemini_%d", time.Now().UnixNano())
}

// SSE streaming.

func (p *Plugin) handleStreamResponse(body io.Reader, requestID string, meta map[string]any, tags map[string]string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var fullContent strings.Builder
	var toolCalls []events.ToolCallRequest
	var finishReason string
	var model string
	var totalUsage apiUsageMetadata
	// Gemini's stream payloads have no native turn ID, so we synthesize one
	// per stream. TUI keys streaming bubbles on TurnID; an empty value would
	// collapse all turns into a single bubble.
	turnID := generateTurnID()
	// pub is the gated path from this stream to the bus. Text goes through it
	// rather than to bus.Emit directly so before:llm.stream.chunk can hold or
	// block a delta before any UI sees it.
	pub := engine.NewStreamPublisher(p.bus, turnID, requestID)
	defer pub.Close()
	toolCallSeq := 0
	// Signatures accumulate across chunks alongside toolCallSeq, so a signature
	// is keyed by the same call ID the ToolCallRequest ends up with even when
	// the parts of a batch straddle chunk boundaries.
	var sigs thoughtSignatures

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk apiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			p.logger.Warn("gemini: parse stream chunk", "error", err)
			continue
		}

		if chunk.ModelVersion != "" {
			model = chunk.ModelVersion
		}
		if chunk.UsageMetadata != nil {
			// Each chunk carries a cumulative usage snapshot; keep latest.
			totalUsage = *chunk.UsageMetadata
		}

		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]
		if cand.FinishReason != "" {
			finishReason = cand.FinishReason
		}

		for _, part := range cand.Content.Parts {
			// Above the switch on purpose. Under streaming Gemini may deliver a
			// signature on a part with empty text and no functionCall, which
			// matches no arm below and would otherwise vanish. See
			// thoughtSignatures.
			sigs.capture(part, toolCallSeq)

			switch {
			case part.Thought && part.Text != "":
				_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: turnID,
					Source:    pluginID,
					Content:   part.Text,
					Phase:     "reasoning",
					Timestamp: time.Now(),
				})

			case part.Text != "":
				// fullContent is the model's text verbatim; the publisher
				// decides what of it reaches a UI. They diverge when a gate
				// redacts or blocks, which is what lets the response-level
				// hook still see what the model actually said.
				fullContent.WriteString(part.Text)
				pub.Text(part.Text)

			case part.FunctionCall != nil:
				args, _ := json.Marshal(part.FunctionCall.Args)
				tc := events.ToolCallRequest{
					ID:        fmt.Sprintf("call_%d_%s", toolCallSeq, part.FunctionCall.Name),
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				}
				toolCalls = append(toolCalls, tc)
				pub.ToolCall(&tc)
				toolCallSeq++

			case part.ExecutableCode != nil:
				code := part.ExecutableCode.Code
				lang := part.ExecutableCode.Language
				snippet := fmt.Sprintf("\n```%s\n%s\n```\n", strings.ToLower(lang), code)
				fullContent.WriteString(snippet)
				pub.Text(snippet)
				_ = p.bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "_gemini_code_execution",
					Arguments: map[string]any{"language": lang, "code": code},
				})

			case part.CodeExecutionResult != nil:
				out := part.CodeExecutionResult.Output
				outcome := part.CodeExecutionResult.Outcome
				snippet := fmt.Sprintf("\n```output\n%s\n```\n", out)
				fullContent.WriteString(snippet)
				pub.Text(snippet)
				toolResult := events.ToolResult{SchemaVersion: events.ToolResultVersion, Name: "_gemini_code_execution",
					Output: out,
				}
				if outcome != "OUTCOME_OK" {
					toolResult.Error = outcome
				}
				_ = p.bus.Emit("tool.result", toolResult)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		p.emitError(fmt.Errorf("gemini: stream read error: %w", err))
	}

	finalUsage := events.Usage{
		PromptTokens:     totalUsage.PromptTokenCount,
		CompletionTokens: totalUsage.CandidatesTokenCount,
		TotalTokens:      totalUsage.TotalTokenCount,
		ReasoningTokens:  totalUsage.ThoughtsTokenCount,
		CachedTokens:     totalUsage.CachedContentTokenCount,
	}
	if mb := geminiModalityBreakdown(&totalUsage); len(mb) > 0 {
		finalUsage.ModalityBreakdown = mb
	}

	// Settle the gated stream before announcing the end of it: Close gives a
	// handler one last look at any tail it was holding, and releasing that
	// tail after llm.stream.end would arrive out of order. The deferred Close
	// above is the error-path backstop; this one fixes the ordering.
	pub.Close()

	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{SchemaVersion: events.StreamEndVersion, TurnID: turnID,
		FinishReason: finishReason,
		Usage:        finalUsage,
	})

	engine.PublishLLMResponse(p.bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		RequestID:     requestID,
		Content:       fullContent.String(),
		ToolCalls:     toolCalls,
		Usage:         finalUsage,
		CostUSD:       p.costForModel(model, finalUsage),
		Model:         model,
		FinishReason:  finishReason,
		// Request-passthrough metadata merges onto the captured signatures.
		Metadata: mergeMetadata(sigs.metadata(), meta),
		Tags:     tags,
	})
}

// costForModel returns the USD cost for a response from the given model.
//
// Google appends "-001" / "-latest" / date suffixes to model IDs, so an
// exact-match miss falls back to the longest-known-prefix match against the
// table. Length-ordering avoids a "gemini-1.5" entry shadowing
// "gemini-1.5-flash" when both are present.
func (p *Plugin) costForModel(model string, usage events.Usage) float64 {
	if cost := p.pricing.Calc(model, usage); cost > 0 {
		return cost
	}
	if _, ok := p.pricing.Get(model); ok {
		// Exact match exists but cost is zero (e.g. free model). Done.
		return 0
	}
	var bestPrefix string
	for _, candidate := range p.pricing.Models() {
		if strings.HasPrefix(model, candidate) && len(candidate) > len(bestPrefix) {
			bestPrefix = candidate
		}
	}
	if bestPrefix != "" {
		return p.pricing.Calc(bestPrefix, usage)
	}
	return 0
}

func (p *Plugin) debugLog(label string, data []byte) {
	if !p.debug || p.session == nil {
		return
	}
	p.mu.Lock()
	seq := p.requestSeq
	p.mu.Unlock()

	filename := fmt.Sprintf("plugins/%s/%04d_%s.json", pluginID, seq, label)
	if err := p.session.WriteFile(filename, data); err != nil {
		p.logger.Warn("failed to write debug log", "file", filename, "error", err)
	}
}

func (p *Plugin) emitError(err error) {
	p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Source: pluginID,
		Err:   err,
		Fatal: false,
	})
}

func (p *Plugin) emitErrorInfo(info events.ErrorInfo) {
	info.Source = pluginID
	p.logger.Error(info.Err.Error())

	result, vErr := p.bus.EmitVetoable("before:core.error", &info)
	if vErr != nil {
		p.logger.Error("failed to emit before:core.error", "error", vErr)
		return
	}
	if result.Vetoed {
		return
	}
	_ = p.bus.Emit("core.error", info)
}
