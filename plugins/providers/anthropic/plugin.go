package anthropic

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
	pluginID   = "nexus.llm.anthropic"
	pluginName = "Anthropic LLM Provider"
	version    = "0.1.0"

	// defaultMaxTokens is the floor max_tokens applied when neither the
	// request, the request's role, nor the default role specifies one.
	// Picked to be large enough for a typical agent turn but well below
	// every 4.x model's per-request ceiling.
	defaultMaxTokens = 4096
)

// Pricing tables and cost calculation live in pkg/engine/pricing/ — providers
// share a single source of truth so the cost CLI, multi-dim budget gate, and
// router can reason about every model uniformly.

// Plugin implements the Anthropic LLM provider.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	models  *engine.ModelRegistry
	session *engine.SessionWorkspace
	replay  *engine.ReplayState

	// liveCalls counts API calls that actually hit the wire (or would
	// have hit the wire — the value increments after the replay short-
	// circuit check). Tests assert this stays at 0 during replay.
	liveCalls atomic.Uint64

	auth              *authState
	client            *http.Client
	prompts           *engine.PromptRegistry
	unsubs            []func()
	debug             bool
	retry             retryConfig
	cache             cacheConfig
	thinking          thinkingConfig
	multimodal        multimodalConfig
	files             filesConfig
	citations         citationsConfig
	structuredOutputs structuredOutputsConfig
	pricing           *pricing.Table // merged: config overrides + embedded defaults

	// filesAPIURL is the production endpoint by default; tests override.
	filesAPIURL string
	fileCache   *fileCache

	mu                 sync.Mutex
	currentRequestMeta map[string]any
	currentRequestTags map[string]string  // copied to llm.response for cost attribution
	cancelFunc         context.CancelFunc // cancels the in-flight HTTP request
	requestSeq         int                // monotonic counter for debug log filenames
	sessionFileIDs     []string           // file_ids uploaded this session (for delete_on_shutdown)
}

// New creates a new Anthropic provider plugin.
func New() engine.Plugin {
	return &Plugin{}
}

// LiveCalls returns the number of llm.request handler invocations that
// passed the replay short-circuit (i.e. would have hit the API). Tests
// read this to assert zero calls during deterministic replay.
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
	p.replay = ctx.Replay

	if debug, ok := ctx.Config["debug"].(bool); ok {
		p.debug = debug
	}

	// Resolve auth mode: api_key (default), bedrock, or vertex. Backwards
	// compatible with the legacy api_key / api_key_env top-level keys; the
	// authState struct centralizes URL construction, body version markers,
	// and per-request signing.
	auth, err := parseAuthConfig(ctx.Config)
	if err != nil {
		return err
	}
	p.auth = auth
	p.logger.Info("anthropic auth resolved", "mode", string(auth.mode))

	p.client = &http.Client{
		Timeout: 5 * time.Minute,
	}
	p.prompts = ctx.Prompts

	p.pricing = parsePricingConfig(ctx.Config)

	p.cache = parseCacheConfig(ctx.Config)
	if p.cache.Enabled {
		p.logger.Info("prompt caching enabled",
			"system", p.cache.System,
			"tools", p.cache.Tools,
			"message_prefix", p.cache.MessagePrefix,
			"ttl", p.cache.TTL,
		)
	}

	p.thinking = parseThinkingConfig(ctx.Config)
	if p.thinking.Enabled {
		p.logger.Info("extended thinking enabled",
			"budget_tokens", p.thinking.BudgetTokens,
			"include_thoughts", p.thinking.IncludeThoughts,
		)
	}

	p.multimodal = parseMultimodalConfig(ctx.Config)
	if p.multimodal.PDFBeta {
		p.logger.Info("multimodal pdf_beta header enabled (pdfs-2024-09-25)")
	}

	p.citations = parseCitationsConfig(ctx.Config)
	if p.citations.Enabled {
		p.logger.Info("native citations enabled (document blocks will request citations)")
	}

	p.structuredOutputs = parseStructuredOutputsConfig(ctx.Config)
	if p.structuredOutputs.Mode == "native" {
		p.logger.Info("structured outputs using native response_format mode",
			"beta_header", p.structuredOutputs.BetaHeader,
		)
	}

	p.files = parseFilesConfig(ctx.Config)
	p.filesAPIURL = filesAPIBaseURL
	if p.files.Enabled {
		p.fileCache = newFileCache()
		p.logger.Info("files API enabled",
			"upload_threshold", p.files.UploadThreshold,
			"cache_uploads", p.files.CacheUploads,
			"delete_on_shutdown", p.files.DeleteOnShutdown,
		)
	}

	p.retry = parseRetryConfig(ctx.Config)
	if p.retry.Enabled {
		p.logger.Info("retry enabled",
			"max_retries", p.retry.MaxRetries,
			"backoff", string(p.retry.Backoff),
			"initial_delay", p.retry.InitialDelay,
			"max_delay", p.retry.MaxDelay,
		)
	}

	// Register event handlers.
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

func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.files.Enabled && p.files.DeleteOnShutdown {
		ids := p.snapshotSessionFileIDs()
		for _, id := range ids {
			if err := p.deleteFile(ctx, id); err != nil {
				// Best-effort: log and continue. The 30-day retention window
				// makes leaked files a soft cost, not a correctness issue.
				p.logger.Warn("anthropic: failed to delete session file", "file_id", id, "error", err)
			}
		}
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
		"llm.response",
		"llm.stream.chunk",
		"llm.stream.end",
		"thinking.step",
		"before:core.error",
		"core.error",
	}
}

// handleEvent dispatches incoming events.
func (p *Plugin) handleEvent(event engine.Event[any]) {
	if event.Type != "llm.request" {
		return
	}
	req, ok := event.Payload.(events.LLMRequest)
	if !ok {
		p.emitError(fmt.Errorf("anthropic: invalid llm.request payload type: %T", event.Payload))
		return
	}
	p.handleRequest(req)
}

func (p *Plugin) handleCancel(event engine.Event[any]) {
	p.mu.Lock()
	cancel := p.cancelFunc
	p.cancelFunc = nil
	p.mu.Unlock()

	if cancel != nil {
		p.logger.Info("cancelling in-flight LLM request")
		cancel()
	}
}

func (p *Plugin) handleRequest(req events.LLMRequest) {
	// If a specific provider is targeted (e.g. by fallback plugin), skip
	// if it's not us.
	if target, ok := req.Metadata["_target_provider"].(string); ok && target != pluginID {
		return
	}

	// Replay short-circuit: pop the next stashed llm.response from the
	// journal queue and emit it instead of calling the API. The
	// coordinator seeded these from the source journal in seq order, so
	// the Nth live llm.request consumes the Nth journaled llm.response.
	if p.replay != nil && p.replay.Active() {
		raw, ok := p.replay.Pop("llm.response")
		if !ok {
			p.logger.Warn("anthropic: replay stash empty for llm.request — emitting empty response")
			_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: req.Model})
			return
		}
		resp, err := journal.PayloadAs[events.LLMResponse](raw)
		if err != nil {
			p.logger.Warn("anthropic: replay payload decode failed", "error", err)
			_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: req.Model})
			return
		}
		_ = p.bus.Emit("llm.response", resp)
		return
	}

	p.liveCalls.Add(1)

	model := req.Model
	maxTokens := req.MaxTokens

	// Resolve model role if no explicit model is set.
	if model == "" && p.models != nil {
		if cfg, ok := p.models.Resolve(req.Role); ok {
			// If the resolved config targets a different provider, skip this request.
			if cfg.Provider != "" && cfg.Provider != pluginID {
				return
			}
			if cfg.Model != "" {
				model = cfg.Model
			}
			if maxTokens == 0 && cfg.MaxTokens > 0 {
				maxTokens = cfg.MaxTokens
			}
		}
	}

	// Fall back to default model role.
	if model == "" && p.models != nil {
		def := p.models.Default()
		if def.Provider == "" || def.Provider == pluginID {
			model = def.Model
			if maxTokens == 0 {
				maxTokens = def.MaxTokens
			}
		}
	}

	// max_tokens may still be 0 — common when the router (idea 09) rewrote
	// req.Model to a concrete id without touching MaxTokens, so the
	// model-resolution branches above were skipped. Try the role's
	// max_tokens, then the default role's, then a safe constant. The
	// Anthropic API rejects max_tokens=0 with "stream cannot be true when
	// max tokens is 0", so we must never let that through.
	if maxTokens == 0 && p.models != nil && req.Role != "" {
		if cfg, ok := p.models.Resolve(req.Role); ok && cfg.MaxTokens > 0 {
			maxTokens = cfg.MaxTokens
		}
	}
	if maxTokens == 0 && p.models != nil {
		if def := p.models.Default(); def.MaxTokens > 0 {
			maxTokens = def.MaxTokens
		}
	}
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	p.logger.Debug("resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)

	// Files API preflight: when enabled, swap oversize Data parts for file_ids
	// before serializing the request body. We replace req.Messages locally
	// (NOT via mutation) — the caller's slice is untouched.
	preflightCtx, preflightCancel := context.WithCancel(context.Background())
	if p.files.Enabled {
		newMsgs, err := p.preuploadParts(preflightCtx, req.Messages)
		if err != nil {
			preflightCancel()
			p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("anthropic: files preflight failed: %w", err),
				Retryable:   false,
				RequestMeta: req.Metadata,
			})
			return
		}
		req.Messages = newMsgs
	}
	preflightCancel()

	body := p.buildRequestBody(model, maxTokens, req)

	jsonBody, err := json.Marshal(body)
	if err != nil {
		p.emitError(fmt.Errorf("anthropic: failed to marshal request: %w", err))
		return
	}

	if p.debug {
		p.mu.Lock()
		p.requestSeq++
		p.mu.Unlock()
		p.debugLog("request", jsonBody)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancelFunc = cancel
	p.mu.Unlock()

	requestURL := p.auth.buildURL(model, req.Stream)

	makeReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", requestURL, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		// applyAuth attaches the right credential headers per mode (and signs
		// the body for Bedrock SigV4). Bedrock signatures depend on the
		// timestamp, so the closure recomputes them on every retry.
		if err := p.auth.applyAuth(reqCtx, httpReq, jsonBody, p.client); err != nil {
			return nil, err
		}
		// Beta-flag aggregation merges the plugin's standing flags (cache 1h,
		// files API, pdf_beta) with any per-request flags stamped by
		// server-tool plugins via req.Metadata["_anthropic_beta_headers"].
		// Sent verbatim regardless of mode — Bedrock and Vertex gate beta
		// features independently of the direct API, so a flag this plugin
		// considers active may be rejected (or silently ignored) by the
		// chosen backend. Surface those mismatches as explicit 400s rather
		// than trying to predict per-feature parity here.
		if flags := p.betaFlags(req.Metadata); flags != "" {
			httpReq.Header.Set("anthropic-beta", flags)
		}
		return httpReq, nil
	}

	resp, err := p.doWithRetry(reqCtx, makeReq)
	if err != nil {
		p.mu.Lock()
		p.cancelFunc = nil
		p.mu.Unlock()
		if reqCtx.Err() == context.Canceled {
			p.logger.Info("LLM request cancelled")
			return
		}
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("anthropic: HTTP request failed: %w", err),
			Retryable:        true,
			RetriesExhausted: true,
			RequestMeta:      req.Metadata,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("anthropic: API returned status %d: %s", resp.StatusCode, string(respBody)),
			Retryable:   false,
			RequestMeta: req.Metadata,
		})
		return
	}

	// Store request metadata for passthrough to response.
	// Flag simulated structured output so downstream consumers know enforcement was attempted.
	meta := req.Metadata
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" {
		if meta == nil {
			meta = make(map[string]any)
		}
		meta["_structured_output"] = true
	}
	p.mu.Lock()
	p.currentRequestMeta = meta
	p.currentRequestTags = req.Tags
	p.mu.Unlock()

	var responseBody io.Reader = resp.Body
	var debugBuf *bytes.Buffer
	if p.debug {
		debugBuf = new(bytes.Buffer)
		responseBody = io.TeeReader(resp.Body, debugBuf)
	}

	if req.Stream {
		p.handleStreamResponse(responseBody)
	} else {
		p.handleSyncResponse(responseBody)
	}

	if debugBuf != nil {
		p.debugLog("response", debugBuf.Bytes())
	}

	p.mu.Lock()
	p.cancelFunc = nil
	p.mu.Unlock()
}

// buildRequestBody constructs the Anthropic API request body.
func (p *Plugin) buildRequestBody(model string, maxTokens int, req events.LLMRequest) map[string]any {
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"stream":     req.Stream,
	}

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	// Extended thinking. When enabled this strips any non-1 temperature
	// (Anthropic requires temp=1) and adds the thinking object to body.
	applyThinking(body, p.thinking, p.logger)

	// Extract system prompt and build messages.
	var systemPrompt string
	var apiMessages []map[string]any

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}

		apiMsg := p.convertMessage(msg)
		if apiMsg != nil {
			apiMessages = append(apiMessages, apiMsg)
		}
	}

	if p.prompts != nil {
		systemPrompt = p.prompts.Apply(systemPrompt)
	}
	if systemPrompt != "" {
		body["system"] = systemPrompt
	}
	body["messages"] = apiMessages

	// Apply tool filtering. Mode "none" strips all tools.
	filteredTools := applyToolFilter(req.Tools, req.ToolFilter)
	if req.ToolChoice != nil && req.ToolChoice.Mode == "none" {
		filteredTools = nil
	}

	// Convert tool definitions.
	if len(filteredTools) > 0 {
		var tools []map[string]any
		for _, t := range filteredTools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = tools
	}

	// Server-tool injection hook. Plugins like nexus.tool.anthropic_native.bash
	// stamp `req.Metadata["_anthropic_extra_tools"]` with already-Anthropic-shaped
	// tool entries (e.g. {"type":"bash_20250124","name":"bash"}). They're
	// passed through verbatim — no schema conversion. Coexists with both
	// client-defined tools and the structured-output synthetic tool below.
	p.appendExtraTools(body, req.Metadata)

	// Structured output handling. Two modes:
	//
	//   - "tool" (default): simulate via a synthetic `_structured_output` tool
	//     and force tool_choice on it. Compatible with every Claude model that
	//     supports tool use, but clobbers the agent's tool_choice.
	//   - "native": use Anthropic's top-level `response_format` field. The
	//     model returns the JSON value as plain text, so tool_choice is left
	//     untouched and parallel tool use stays available.
	//
	// When no json_schema response_format is set, both modes fall through to
	// the normal tool_choice mapping below.
	rf := req.ResponseFormat
	hasJSONSchema := rf != nil && rf.Type == "json_schema" && rf.Schema != nil

	switch {
	case hasJSONSchema && p.structuredOutputs.Mode == "native":
		body["response_format"] = map[string]any{
			"type":   "json_schema",
			"schema": rf.Schema,
		}
		// Native mode does not override tool_choice — preserve whatever the
		// caller asked for so real tools remain usable in parallel.
		if tc := resolveToolChoice(req.ToolChoice, filteredTools); tc != nil {
			body["tool_choice"] = tc
		}

	case hasJSONSchema:
		// Tool-as-schema simulation (default).
		syntheticTool := map[string]any{
			"name":         "_structured_output",
			"description":  "Return structured output matching the required schema.",
			"input_schema": rf.Schema,
		}
		if tools, ok := body["tools"].([]map[string]any); ok {
			body["tools"] = append(tools, syntheticTool)
		} else {
			body["tools"] = []map[string]any{syntheticTool}
		}
		body["tool_choice"] = map[string]any{
			"type": "tool",
			"name": "_structured_output",
		}

	default:
		// Map tool choice to Anthropic API format (only when not overridden by structured output).
		if tc := resolveToolChoice(req.ToolChoice, filteredTools); tc != nil {
			body["tool_choice"] = tc
		}
	}

	// Mark cacheable prefix segments (system, last tool, leading user msgs) per
	// configured policy. No-op when caching is disabled.
	applyCacheControl(body, p.cache, p.logger)

	// Bedrock/Vertex want anthropic_version inside the body (the direct API
	// uses an HTTP header instead). Bedrock additionally rejects bodies that
	// carry the model field — the model id lives in the URL path there.
	if p.auth != nil {
		if v := p.auth.bodyVersionField(); v != "" {
			body["anthropic_version"] = v
		}
		if p.auth.stripModelFromBody() {
			delete(body, "model")
		}
	}

	return body
}

// convertMessage converts an events.Message to the Anthropic API format.
func (p *Plugin) convertMessage(msg events.Message) map[string]any {
	switch msg.Role {
	case "assistant":
		if len(msg.ToolCalls) > 0 {
			var content []map[string]any

			// Round-trip preservation: when extended thinking emitted blocks
			// on the response that produced these tool calls, those blocks
			// (with their cryptographic signatures) MUST be echoed back as
			// the leading content on this assistant turn. Anthropic rejects
			// the next request with HTTP 400 otherwise.
			content = append(content, prependThinkingBlocks(msg.Metadata)...)

			if msg.Content != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
					input = map[string]any{}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": input,
				})
			}
			return map[string]any{
				"role":    "assistant",
				"content": content,
			}
		}
		return map[string]any{
			"role":    "assistant",
			"content": msg.Content,
		}

	case "tool":
		// Anthropic accepts content blocks inside tool_result.content, so a
		// tool that produced an image (e.g. a screenshot) can pass it inline.
		// On error we log and fall back to the text Content.
		var toolResultContent any = msg.Content
		if len(msg.Parts) > 0 {
			blocks, err := buildContentBlocks(msg, p.citations.Enabled)
			if err != nil {
				p.logger.Error("anthropic: dropping tool-result multimodal parts", "error", err)
			} else {
				toolResultContent = blocks
			}
		}
		return map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     toolResultContent,
				},
			},
		}

	case "user":
		if len(msg.Parts) > 0 {
			blocks, err := buildContentBlocks(msg, p.citations.Enabled)
			if err != nil {
				// Multimodal serialization failed (e.g. oversize image, audio
				// part). Surface the failure via slog and fall back to the
				// text-only path so the request still goes out — silently
				// dropping is worse than a partial send the user can debug.
				p.logger.Error("anthropic: dropping multimodal parts", "error", err)
				return map[string]any{
					"role":    "user",
					"content": msg.Content,
				}
			}
			return map[string]any{
				"role":    "user",
				"content": blocks,
			}
		}
		return map[string]any{
			"role":    "user",
			"content": msg.Content,
		}

	default:
		return map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}
}

// apiResponse represents the Anthropic Messages API non-streaming response.
type apiResponse struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Content    []apiContentBlock `json:"content"`
	Model      string            `json:"model"`
	StopReason string            `json:"stop_reason"`
	Usage      apiUsage          `json:"usage"`
}

type apiContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// Extended thinking fields. Populated when Type is "thinking" or
	// "redacted_thinking". The cryptographic Signature MUST be preserved and
	// echoed back verbatim on the next assistant turn after a tool result, or
	// the API rejects the request with HTTP 400. Data carries the encrypted
	// payload of redacted_thinking blocks (the unencrypted Thinking field is
	// empty in that case).
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	// Native citations. Anthropic populates this on `text` blocks when the
	// originating request included document blocks with citations enabled.
	Citations []apiCitation `json:"citations,omitempty"`

	// Server-side tool result blocks (e.g. `code_execution_tool_result`). These
	// are tool _results_ embedded in the assistant message — Anthropic ran the
	// tool server-side, so there's no client-side execution required. ToolUseID
	// references the matching server-side tool_use ID; Content carries the
	// inner result block (e.g. {"type":"code_execution_result","stdout":"…",
	// "stderr":"…","return_code":0}) as raw JSON so we don't have to model
	// every server-tool's inner shape exhaustively.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// isServerToolResultBlock reports whether the block type is a server-side
// tool result (currently only `code_execution_tool_result`, but Anthropic
// will likely add more — keep this central so we surface them uniformly).
func isServerToolResultBlock(blockType string) bool {
	switch blockType {
	case "code_execution_tool_result":
		return true
	default:
		return false
	}
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (p *Plugin) handleSyncResponse(body io.Reader) {
	var apiResp apiResponse
	if err := json.NewDecoder(body).Decode(&apiResp); err != nil {
		p.emitError(fmt.Errorf("anthropic: failed to decode response: %w", err))
		return
	}

	resp := p.convertAPIResponse(apiResp)

	// Merge passthrough request metadata with any provider-attached fields
	// (e.g. thinking_blocks). Passthrough wins on key collision so
	// upstream-set flags like _structured_output aren't masked, but
	// provider-attached keys are preserved.
	p.mu.Lock()
	meta := p.currentRequestMeta
	tags := p.currentRequestTags
	p.mu.Unlock()
	if resp.Metadata == nil {
		resp.Metadata = meta
	} else {
		for k, v := range meta {
			resp.Metadata[k] = v
		}
	}
	resp.Tags = tags

	if err := p.bus.Emit("llm.response", resp); err != nil {
		p.logger.Error("failed to emit llm.response", "error", err)
	}
}

func (p *Plugin) convertAPIResponse(apiResp apiResponse) events.LLMResponse {
	var content strings.Builder
	var toolCalls []events.ToolCallRequest
	var thinkingBlocks []map[string]any
	var citations []events.Citation
	var serverResults []map[string]any
	thinkingIdx := 0

	for _, block := range apiResp.Content {
		if isServerToolResultBlock(block.Type) {
			// Preserve content verbatim — downstream consumers (e.g. the
			// per-server-tool plugins) decode the inner shape themselves so
			// we don't have to track every Anthropic server tool's wire format
			// here.
			serverResults = append(serverResults, map[string]any{
				"type":        block.Type,
				"tool_use_id": block.ToolUseID,
				"content":     json.RawMessage(block.Content),
			})
			continue
		}

		switch block.Type {
		case "text":
			content.WriteString(block.Text)
			for _, c := range block.Citations {
				citations = append(citations, c.toEvent())
			}

		case "thinking":
			// Preserve the block verbatim (with signature) so the next
			// assistant turn after a tool result can echo it back — Anthropic
			// requires this or rejects with HTTP 400.
			tb := map[string]any{
				"type":      "thinking",
				"thinking":  block.Thinking,
				"signature": block.Signature,
			}
			thinkingBlocks = append(thinkingBlocks, tb)
			if p.thinking.IncludeThoughts && block.Thinking != "" {
				_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: apiResp.ID,
					Source:    pluginID,
					Content:   block.Thinking,
					Phase:     "reasoning",
					Timestamp: time.Now(),
					Index:     thinkingIdx,
				})
				thinkingIdx++
			}

		case "redacted_thinking":
			// Encrypted/redacted thinking. Pass through opaquely; we never
			// surface contents on the bus but the block still has to be
			// echoed back on tool-use turns.
			tb := map[string]any{
				"type": "redacted_thinking",
				"data": block.Data,
			}
			thinkingBlocks = append(thinkingBlocks, tb)

		case "tool_use":
			args := string(block.Input)
			// Unwrap synthetic structured output tool — put args into Content.
			if block.Name == "_structured_output" {
				content.WriteString(args)
				continue
			}
			toolCalls = append(toolCalls, events.ToolCallRequest{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		}
	}

	usage := events.Usage{
		PromptTokens:     apiResp.Usage.InputTokens,
		CompletionTokens: apiResp.Usage.OutputTokens,
		CachedTokens:     apiResp.Usage.CacheReadInputTokens,
		CacheWriteTokens: apiResp.Usage.CacheCreationInputTokens,
		TotalTokens: apiResp.Usage.InputTokens + apiResp.Usage.OutputTokens +
			apiResp.Usage.CacheReadInputTokens + apiResp.Usage.CacheCreationInputTokens,
	}
	// Anthropic bills thinking tokens as part of output_tokens; we surface a
	// best-effort accounting via ReasoningTokens. The API doesn't (yet)
	// separate the thinking portion from the visible output, so this stays at
	// 0 for now — billing impact is captured in CompletionTokens. Plan 02
	// reserves the field for when Anthropic exposes it.

	resp := events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: content.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		CostUSD:      p.costForModel(apiResp.Model, usage),
		Model:        apiResp.Model,
		FinishReason: apiResp.StopReason,
		Citations:    citations,
	}
	if len(thinkingBlocks) > 0 || len(serverResults) > 0 {
		resp.Metadata = map[string]any{}
		if len(thinkingBlocks) > 0 {
			resp.Metadata["thinking_blocks"] = thinkingBlocks
		}
		if len(serverResults) > 0 {
			resp.Metadata["server_tool_results"] = serverResults
		}
	}
	return resp
}

// SSE streaming response handling.

type sseEvent struct {
	Event string
	Data  string
}

// streamState carries the mutable bookkeeping for a single SSE response. It
// replaces a long parameter list on processSSEEvent and gives thinking-block
// accumulation a natural home (one builder + signature per active block,
// finalized into thinkingBlocks at content_block_stop).
type streamState struct {
	fullContent      strings.Builder
	toolCalls        []events.ToolCallRequest
	currentToolCall  *events.ToolCallRequest
	currentToolInput strings.Builder

	// Active thinking block (one at a time; Anthropic sends them serially).
	// blockType is "thinking" or "redacted_thinking" when a thinking block is
	// open, empty otherwise. signature/data fields are populated by
	// signature_delta and the content_block_start event respectively.
	thinkingBlockType string
	thinkingBuilder   strings.Builder
	thinkingSignature string
	thinkingData      string
	thinkingIdx       int
	thinkingBlocks    []map[string]any

	// Citations accumulated during the active text block (flushed to
	// citations on content_block_stop) and the running list across all
	// blocks for the response. Anthropic emits citations only on `text`
	// blocks via citations_delta deltas.
	currentCitations []apiCitation
	citations        []events.Citation

	// Active server-side tool result block (e.g. code_execution_tool_result).
	// serverToolBlockType is set when one is open, empty otherwise. Anthropic's
	// streaming wire shape for these blocks is still in flux at the time of
	// writing (the plugin parses defensively): content_block_start carries
	// type + tool_use_id, deltas accumulate the inner content as raw JSON
	// (likely via input_json_delta or a similarly-shaped delta), and
	// content_block_stop finalizes the entry into serverToolResults.
	serverToolBlockType string
	serverToolUseID     string
	serverToolBuffer    strings.Builder
	serverToolResults   []map[string]any

	usage        apiUsage
	model        string
	finishReason string
	turnID       string
	chunkIndex   int
}

func (p *Plugin) handleStreamResponse(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var currentEvent sseEvent
	st := &streamState{}

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line means end of SSE event; process it.
			if currentEvent.Event != "" {
				p.processSSEEvent(currentEvent, st)
			}
			currentEvent = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			currentEvent.Event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			currentEvent.Data = strings.TrimPrefix(line, "data: ")
		}
	}

	// Process any remaining event.
	if currentEvent.Event != "" {
		p.processSSEEvent(currentEvent, st)
	}

	if err := scanner.Err(); err != nil {
		p.emitError(fmt.Errorf("anthropic: stream read error: %w", err))
	}

	// Build final usage.
	finalUsage := events.Usage{
		PromptTokens:     st.usage.InputTokens,
		CompletionTokens: st.usage.OutputTokens,
		CachedTokens:     st.usage.CacheReadInputTokens,
		CacheWriteTokens: st.usage.CacheCreationInputTokens,
		TotalTokens: st.usage.InputTokens + st.usage.OutputTokens +
			st.usage.CacheReadInputTokens + st.usage.CacheCreationInputTokens,
	}

	// Emit stream end.
	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{SchemaVersion: events.StreamEndVersion, TurnID: st.turnID,
		FinishReason: st.finishReason,
		Usage:        finalUsage,
	})

	// Also emit the complete llm.response for downstream consumers. Merge
	// passthrough request metadata with thinking_blocks captured from the
	// stream — round-trip preservation requires both to coexist.
	p.mu.Lock()
	meta := p.currentRequestMeta
	p.mu.Unlock()

	respMeta := meta
	if len(st.thinkingBlocks) > 0 || len(st.serverToolResults) > 0 {
		respMeta = make(map[string]any, len(meta)+2)
		for k, v := range meta {
			respMeta[k] = v
		}
		if len(st.thinkingBlocks) > 0 {
			respMeta["thinking_blocks"] = st.thinkingBlocks
		}
		if len(st.serverToolResults) > 0 {
			respMeta["server_tool_results"] = st.serverToolResults
		}
	}

	p.mu.Lock()
	tags := p.currentRequestTags
	p.mu.Unlock()
	_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: st.fullContent.String(),
		ToolCalls:    st.toolCalls,
		Usage:        finalUsage,
		CostUSD:      p.costForModel(st.model, finalUsage),
		Model:        st.model,
		FinishReason: st.finishReason,
		Citations:    st.citations,
		Metadata:     respMeta,
		Tags:         tags,
	})
}

func (p *Plugin) processSSEEvent(sse sseEvent, st *streamState) {
	switch sse.Event {
	case "message_start":
		var data struct {
			Message struct {
				ID    string   `json:"id"`
				Model string   `json:"model"`
				Usage apiUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			st.turnID = data.Message.ID
			st.model = data.Message.Model
			st.usage.InputTokens = data.Message.Usage.InputTokens
			st.usage.CacheCreationInputTokens = data.Message.Usage.CacheCreationInputTokens
			st.usage.CacheReadInputTokens = data.Message.Usage.CacheReadInputTokens
		}

	case "content_block_start":
		var data struct {
			Index        int             `json:"index"`
			ContentBlock apiContentBlock `json:"content_block"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			switch {
			case data.ContentBlock.Type == "tool_use":
				st.currentToolCall = &events.ToolCallRequest{
					ID:   data.ContentBlock.ID,
					Name: data.ContentBlock.Name,
				}
				st.currentToolInput.Reset()
			case isServerToolResultBlock(data.ContentBlock.Type):
				// Anthropic may include the full inner content on the
				// content_block_start event itself, or stream it via deltas.
				// Capture whatever's already present and let deltas append to
				// the buffer until content_block_stop.
				st.serverToolBlockType = data.ContentBlock.Type
				st.serverToolUseID = data.ContentBlock.ToolUseID
				st.serverToolBuffer.Reset()
				if len(data.ContentBlock.Content) > 0 {
					st.serverToolBuffer.Write(data.ContentBlock.Content)
				}
			case data.ContentBlock.Type == "thinking" || data.ContentBlock.Type == "redacted_thinking":
				st.thinkingBlockType = data.ContentBlock.Type
				st.thinkingBuilder.Reset()
				st.thinkingSignature = ""
				// redacted_thinking arrives with its full encrypted payload on
				// content_block_start (no deltas); thinking gets streamed via
				// thinking_delta + signature_delta until content_block_stop.
				st.thinkingData = data.ContentBlock.Data
				if data.ContentBlock.Thinking != "" {
					st.thinkingBuilder.WriteString(data.ContentBlock.Thinking)
				}
				if data.ContentBlock.Signature != "" {
					st.thinkingSignature = data.ContentBlock.Signature
				}
			}
		}

	case "content_block_delta":
		var data struct {
			Index int `json:"index"`
			Delta struct {
				Type        string      `json:"type"`
				Text        string      `json:"text,omitempty"`
				PartialJSON string      `json:"partial_json,omitempty"`
				Thinking    string      `json:"thinking,omitempty"`
				Signature   string      `json:"signature,omitempty"`
				Citation    apiCitation `json:"citation,omitempty"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			switch data.Delta.Type {
			case "text_delta":
				st.fullContent.WriteString(data.Delta.Text)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: data.Delta.Text,
					Index:  st.chunkIndex,
					TurnID: st.turnID,
				})
				st.chunkIndex++

			case "citations_delta":
				// Accumulate per-block; flushed into st.citations at
				// content_block_stop so ordering across multiple text
				// blocks is preserved.
				st.currentCitations = append(st.currentCitations, data.Delta.Citation)

			case "thinking_delta":
				st.thinkingBuilder.WriteString(data.Delta.Thinking)
				if p.thinking.IncludeThoughts && data.Delta.Thinking != "" {
					_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: st.turnID,
						Source:    pluginID,
						Content:   data.Delta.Thinking,
						Phase:     "reasoning",
						Timestamp: time.Now(),
						Index:     st.thinkingIdx,
					})
					st.thinkingIdx++
				}

			case "signature_delta":
				// Signatures aren't chunked semantically (a single delta
				// carries the full signature for the active block) but
				// concatenate defensively in case Anthropic ever splits them.
				st.thinkingSignature += data.Delta.Signature

			case "input_json_delta":
				switch {
				case st.currentToolCall != nil:
					st.currentToolInput.WriteString(data.Delta.PartialJSON)
					// Stream structured output tool input as content chunks.
					if st.currentToolCall.Name == "_structured_output" {
						_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: data.Delta.PartialJSON,
							Index:  st.chunkIndex,
							TurnID: st.turnID,
						})
						st.chunkIndex++
					}
				case st.serverToolBlockType != "":
					// Best-effort accumulation: Anthropic's exact streaming
					// shape for server-tool result blocks is still in flux.
					// We assume input_json_delta-style chunks until proven
					// otherwise; the inner content is preserved verbatim.
					st.serverToolBuffer.WriteString(data.Delta.PartialJSON)
				}
			}
		}

	case "content_block_stop":
		switch {
		case st.currentToolCall != nil:
			st.currentToolCall.Arguments = st.currentToolInput.String()

			if st.currentToolCall.Name == "_structured_output" {
				// Unwrap synthetic tool — accumulate into content, not tool calls.
				st.fullContent.WriteString(st.currentToolInput.String())
			} else {
				st.toolCalls = append(st.toolCalls, *st.currentToolCall)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, ToolCall: st.currentToolCall,
					Index:  st.chunkIndex,
					TurnID: st.turnID,
				})
				st.chunkIndex++
			}

			st.currentToolCall = nil
			st.currentToolInput.Reset()

		case st.thinkingBlockType != "":
			// Finalize the active thinking block. We retain the signature
			// verbatim — Anthropic rejects HTTP 400 on the next assistant
			// turn (after a tool result) if it's missing or modified.
			block := map[string]any{"type": st.thinkingBlockType}
			if st.thinkingBlockType == "redacted_thinking" {
				block["data"] = st.thinkingData
			} else {
				block["thinking"] = st.thinkingBuilder.String()
				block["signature"] = st.thinkingSignature
			}
			st.thinkingBlocks = append(st.thinkingBlocks, block)
			st.thinkingBlockType = ""
			st.thinkingBuilder.Reset()
			st.thinkingSignature = ""
			st.thinkingData = ""

		case st.serverToolBlockType != "":
			// Finalize the server-side tool result block. Content is preserved
			// as raw JSON for downstream consumers (per-server-tool plugins
			// decode it). The buffer holds whatever shape Anthropic streamed
			// — we don't validate inner fields here.
			buf := []byte(st.serverToolBuffer.String())
			st.serverToolResults = append(st.serverToolResults, map[string]any{
				"type":        st.serverToolBlockType,
				"tool_use_id": st.serverToolUseID,
				"content":     json.RawMessage(buf),
			})
			st.serverToolBlockType = ""
			st.serverToolUseID = ""
			st.serverToolBuffer.Reset()

		default:
			// Text block (or any other non-tool, non-thinking block): flush
			// any citations accumulated during its content_block_delta runs.
			if len(st.currentCitations) > 0 {
				for _, c := range st.currentCitations {
					st.citations = append(st.citations, c.toEvent())
				}
				st.currentCitations = nil
			}
		}

	case "message_delta":
		var data struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage apiUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			st.finishReason = data.Delta.StopReason
			if data.Usage.OutputTokens > 0 {
				st.usage.OutputTokens = data.Usage.OutputTokens
			}
			// message_delta may carry an updated cache snapshot (Anthropic
			// sometimes finalizes counts here). Prefer the larger value so
			// we don't regress message_start totals.
			if data.Usage.CacheCreationInputTokens > st.usage.CacheCreationInputTokens {
				st.usage.CacheCreationInputTokens = data.Usage.CacheCreationInputTokens
			}
			if data.Usage.CacheReadInputTokens > st.usage.CacheReadInputTokens {
				st.usage.CacheReadInputTokens = data.Usage.CacheReadInputTokens
			}
		}

	case "message_stop":
		// End of message; handled after the loop.

	case "ping":
		// Keepalive; ignore.

	case "error":
		var data struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			p.emitError(fmt.Errorf("anthropic: stream error: %s: %s", data.Error.Type, data.Error.Message))
		}
	}
}

// parsePricingConfig builds the merged price table from embedded defaults
// plus optional `pricing:` overrides under the plugin's config block.
//
//	pricing:
//	  claude-sonnet-4-6-20250514:
//	    input_per_million: 3.0
//	    output_per_million: 15.0
//	    cache_read_per_million: 0.30
//	    cache_write_5m_per_million: 3.75
//	    cache_write_1h_per_million: 6.0
//
// Cache rates are optional; calc derives them when unset (read 0.10×,
// write 5m 1.25×, write 1h 2.0×).
func parsePricingConfig(cfg map[string]any) *pricing.Table {
	tbl := pricing.DefaultsFor(pricing.ProviderAnthropic)
	if raw, ok := cfg["pricing"].(map[string]any); ok {
		tbl.Merge(raw)
	}
	return tbl
}

// costForModel returns the USD cost for a response from the given model.
func (p *Plugin) costForModel(model string, usage events.Usage) float64 {
	return p.pricing.Calc(model, usage)
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
		p.logger.Error("failed to write debug log", "file", filename, "error", err)
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

	// Vetoable gate: fallback plugin can intercept and suppress.
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
