package openai

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
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.llm.openai"
	pluginName = "OpenAI LLM Provider"
	version    = "0.1.0"
	apiURL     = "https://api.openai.com/v1/chat/completions"
)

// modelPricing holds per-model token rates in USD per million tokens.
type modelPricing struct {
	InputPerMillion     float64
	OutputPerMillion    float64
	CacheReadPerMillion float64 // 0 → derived as InputPerMillion * 0.5
}

// defaultPricing is the embedded fallback pricing table. Updated via patch
// releases when providers change rates. Config override always wins.
var defaultPricing = map[string]modelPricing{
	"gpt-4o":            {InputPerMillion: 2.50, OutputPerMillion: 10.0},
	"gpt-4o-mini":       {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	"gpt-4o-2024-11-20": {InputPerMillion: 2.50, OutputPerMillion: 10.0},
	"gpt-4-turbo":       {InputPerMillion: 10.0, OutputPerMillion: 30.0},
	"gpt-4":             {InputPerMillion: 30.0, OutputPerMillion: 60.0},
	"gpt-3.5-turbo":     {InputPerMillion: 0.50, OutputPerMillion: 1.50},
	"o1":                {InputPerMillion: 15.0, OutputPerMillion: 60.0},
	"o1-mini":           {InputPerMillion: 3.0, OutputPerMillion: 12.0},
	"o3":                {InputPerMillion: 10.0, OutputPerMillion: 40.0},
	"o3-mini":           {InputPerMillion: 1.10, OutputPerMillion: 4.40},
	"o4-mini":           {InputPerMillion: 1.10, OutputPerMillion: 4.40},
}

// calculateCost computes the USD cost for a single LLM call.
//
// OpenAI auto-caches eligible prompt prefixes (≥1024 tokens) and bills the
// cached portion at half the input rate. We split prompt tokens into the
// plain (cache miss) and cached portions and bill each at its own rate.
func calculateCost(usage events.Usage, rates modelPricing) float64 {
	cachedRate := rates.CacheReadPerMillion
	if cachedRate == 0 {
		cachedRate = rates.InputPerMillion * 0.5
	}

	plainInput := usage.PromptTokens - usage.CachedTokens
	if plainInput < 0 {
		plainInput = 0
	}

	return float64(plainInput)/1_000_000*rates.InputPerMillion +
		float64(usage.CachedTokens)/1_000_000*cachedRate +
		float64(usage.CompletionTokens)/1_000_000*rates.OutputPerMillion
}

// Plugin implements the OpenAI LLM provider.
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
	pricing map[string]modelPricing // merged: config overrides + embedded defaults

	reasoning      reasoningConfig
	forceReasoning bool

	multimodal multimodalConfig

	files       filesConfig
	filesAPIURL string
	fileCache   *fileCache

	mu                 sync.Mutex
	currentRequestMeta map[string]any
	cancelFunc         context.CancelFunc
	requestSeq         int
	sessionFileIDs     []string // file_ids uploaded this session (for delete_on_shutdown)
}

// New creates a new OpenAI provider plugin.
func New() engine.Plugin {
	return &Plugin{}
}

// LiveCalls returns the number of llm.request handler invocations that
// passed the replay short-circuit. Tests assert zero during replay.
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

	// Auth: openai (default), azure_key, or azure_aad. parseAuthConfig is
	// backwards compatible — when auth_mode is unset, the legacy
	// api_key / api_key_env / base_url top-level keys keep working.
	auth, err := parseAuthConfig(ctx.Config)
	if err != nil {
		return err
	}
	p.auth = auth

	// Files API stays on the public OpenAI endpoint regardless of mode (Azure
	// has a different Files API surface and not all plugins need it). When
	// running in Azure mode, we still need an OpenAI api_key for Files; warn
	// once if the Files API is enabled but no key is available.
	if (p.auth.mode == authModeAzureKey || p.auth.mode == authModeAzureAAD) && p.auth.apiKey == "" {
		ctx.Logger.Warn("openai: Files API will be unavailable in Azure mode without a top-level api_key (Files API stays on api.openai.com)")
	}

	p.client = &http.Client{
		Timeout: 5 * time.Minute,
	}
	p.prompts = ctx.Prompts

	p.pricing = parsePricingConfig(ctx.Config)

	p.reasoning = parseReasoningConfig(ctx.Config)
	if v, ok := ctx.Config["force_reasoning"].(bool); ok {
		p.forceReasoning = v
	}

	p.multimodal = parseMultimodalConfig(ctx.Config)

	p.files = parseFilesConfig(ctx.Config)
	p.filesAPIURL = filesAPIBaseURL
	if p.files.Enabled {
		p.fileCache = newFileCache()
		p.logger.Info("files API enabled",
			"purpose", p.files.Purpose,
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
				// Best-effort: log and continue. A leaked file_id is a soft
				// cost (org storage), not a correctness issue.
				p.logger.Warn("openai: failed to delete session file", "file_id", id, "error", err)
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
		p.emitError(fmt.Errorf("openai: invalid llm.request payload type: %T", event.Payload))
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

	if p.replay != nil && p.replay.Active() {
		raw, ok := p.replay.Pop("llm.response")
		if !ok {
			p.logger.Warn("openai: replay stash empty for llm.request — emitting empty response")
			_ = p.bus.Emit("llm.response", events.LLMResponse{Model: req.Model})
			return
		}
		resp, err := journal.PayloadAs[events.LLMResponse](raw)
		if err != nil {
			p.logger.Warn("openai: replay payload decode failed", "error", err)
			_ = p.bus.Emit("llm.response", events.LLMResponse{Model: req.Model})
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

	p.logger.Debug("resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)

	// Files API preflight: when enabled, upload file-type Data parts and
	// swap in the returned file_id before serializing the request body. We
	// replace req.Messages locally (not via mutation) — the caller's slice
	// stays untouched.
	preflightCtx, preflightCancel := context.WithCancel(context.Background())
	if p.files.Enabled {
		newMsgs, err := p.preuploadParts(preflightCtx, req.Messages)
		if err != nil {
			preflightCancel()
			p.emitErrorInfo(events.ErrorInfo{
				Err:         fmt.Errorf("openai: files preflight failed: %w", err),
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
		p.emitError(fmt.Errorf("openai: failed to marshal request: %w", err))
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

	makeReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", p.auth.buildURL(), bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		if err := p.auth.applyAuth(reqCtx, httpReq, p.client); err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
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
		p.emitErrorInfo(events.ErrorInfo{
			Err:              fmt.Errorf("openai: HTTP request failed: %w", err),
			Retryable:        true,
			RetriesExhausted: true,
			RequestMeta:      req.Metadata,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		p.emitErrorInfo(events.ErrorInfo{
			Err:         fmt.Errorf("openai: API returned status %d: %s", resp.StatusCode, string(respBody)),
			Retryable:   false,
			RequestMeta: req.Metadata,
		})
		return
	}

	// Store request metadata for passthrough to response.
	// Flag native structured output so downstream consumers know enforcement was used.
	meta := req.Metadata
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "text" {
		if meta == nil {
			meta = make(map[string]any)
		}
		meta["_structured_output"] = true
	}
	p.mu.Lock()
	p.currentRequestMeta = meta
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

// buildRequestBody constructs the OpenAI Chat Completions API request body.
func (p *Plugin) buildRequestBody(model string, maxTokens int, req events.LLMRequest) map[string]any {
	body := map[string]any{
		"max_tokens": maxTokens,
		"stream":     req.Stream,
	}
	// Azure encodes the deployment in the URL; including model in the body
	// is redundant (and rejected by some api-versions). Public OpenAI
	// requires it.
	if p.auth == nil || !p.auth.stripModelFromBody() {
		body["model"] = model
	}

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	if req.Stream {
		// Request usage stats in streaming mode.
		body["stream_options"] = map[string]any{
			"include_usage": true,
		}
	}

	// Build messages. OpenAI keeps system messages inline.
	var apiMessages []map[string]any
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			content := msg.Content
			if p.prompts != nil {
				content = p.prompts.Apply(content)
			}
			apiMessages = append(apiMessages, map[string]any{
				"role":    "system",
				"content": content,
			})
			continue
		}
		apiMsg := p.convertMessage(msg)
		if apiMsg != nil {
			apiMessages = append(apiMessages, apiMsg)
		}
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
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		body["tools"] = tools
	}

	// Map tool choice to OpenAI API format.
	if tc := resolveToolChoice(req.ToolChoice, filteredTools); tc != nil {
		body["tool_choice"] = tc
	}

	// Map structured output response format.
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			body["response_format"] = map[string]any{
				"type": "json_object",
			}
		case "json_schema":
			schema := map[string]any{
				"name":   rf.Name,
				"schema": rf.Schema,
				"strict": rf.Strict,
			}
			body["response_format"] = map[string]any{
				"type":        "json_schema",
				"json_schema": schema,
			}
		}
		// "text" is OpenAI default — no field needed.
	}

	// Predicted outputs (OpenAI-only): when the agent has a known-content
	// guess (rewrite this paragraph, fix this line), pass it through as
	// prediction.content so the model returns near-instantly for unchanged
	// portions. applyReasoning strips this for o-series / gpt-5-thinking
	// models since they reject the field.
	if req.Prediction != "" {
		body["prediction"] = map[string]any{
			"type":    "content",
			"content": req.Prediction,
		}
	}

	// Reasoning-model handling runs last so any fields about to be stripped
	// (temperature, top_p, etc.) have already been written. NOTE: this stays
	// on /v1/chat/completions; the /v1/responses endpoint exposes richer
	// reasoning controls (summary streaming) and is left for a future plan.
	applyReasoning(body, model, p.reasoning, p.forceReasoning, p.logger)

	return body
}

// convertMessage converts an events.Message to the OpenAI API format.
//
// When the message carries multimodal Parts, the content is serialized as
// OpenAI's content-array form (text/image_url/input_audio/file blocks). On
// any conversion error the provider logs and falls back to the bare-string
// Content path so a malformed image part doesn't kill the whole turn.
func (p *Plugin) convertMessage(msg events.Message) map[string]any {
	switch msg.Role {
	case "assistant":
		m := map[string]any{
			"role": "assistant",
		}
		if parts, err := buildContentParts(msg, p.multimodal); err != nil {
			p.logger.Warn("openai: failed to build assistant content parts; falling back to text", "error", err)
			if msg.Content != "" {
				m["content"] = msg.Content
			}
		} else if parts != nil {
			m["content"] = parts
		} else if msg.Content != "" {
			m["content"] = msg.Content
		}
		if len(msg.ToolCalls) > 0 {
			var toolCalls []map[string]any
			for _, tc := range msg.ToolCalls {
				toolCalls = append(toolCalls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			m["tool_calls"] = toolCalls
		}
		return m

	case "tool":
		m := map[string]any{
			"role":         "tool",
			"tool_call_id": msg.ToolCallID,
		}
		if parts, err := buildContentParts(msg, p.multimodal); err != nil {
			p.logger.Warn("openai: failed to build tool content parts; falling back to text", "error", err)
			m["content"] = msg.Content
		} else if parts != nil {
			m["content"] = parts
		} else {
			m["content"] = msg.Content
		}
		return m

	case "user":
		m := map[string]any{
			"role": "user",
		}
		if parts, err := buildContentParts(msg, p.multimodal); err != nil {
			p.logger.Warn("openai: failed to build user content parts; falling back to text", "error", err)
			m["content"] = msg.Content
		} else if parts != nil {
			m["content"] = parts
		} else {
			m["content"] = msg.Content
		}
		return m

	default:
		return map[string]any{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}
}

// OpenAI API response types.

type apiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   apiUsage    `json:"usage"`
}

type apiChoice struct {
	Index        int        `json:"index"`
	Message      apiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type apiMessage struct {
	Role      string        `json:"role"`
	Content   *string       `json:"content"`
	ToolCalls []apiToolCall `json:"tool_calls,omitempty"`
}

type apiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
		AudioTokens  int `json:"audio_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens          int `json:"reasoning_tokens"`
		AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
		RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
		AudioTokens              int `json:"audio_tokens"`
	} `json:"completion_tokens_details"`
}

func (p *Plugin) handleSyncResponse(body io.Reader) {
	var apiResp apiResponse
	if err := json.NewDecoder(body).Decode(&apiResp); err != nil {
		p.emitError(fmt.Errorf("openai: failed to decode response: %w", err))
		return
	}

	resp := p.convertAPIResponse(apiResp)

	// Merge request-passthrough metadata (e.g. _structured_output) onto any
	// metadata convertAPIResponse already attached (e.g. prediction_acceptance).
	p.mu.Lock()
	resp.Metadata = mergeMetadata(resp.Metadata, p.currentRequestMeta)
	p.mu.Unlock()

	if err := p.bus.Emit("llm.response", resp); err != nil {
		p.logger.Error("failed to emit llm.response", "error", err)
	}
}

// mergeMetadata returns a map containing all keys from `into` plus all keys
// from `from`. Either input may be nil. Keys in `from` overwrite duplicates
// in `into`. Returns nil only when both inputs are nil/empty.
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

func (p *Plugin) convertAPIResponse(apiResp apiResponse) events.LLMResponse {
	// Reasoning tokens (o-series, gpt-5-thinking) are billed by OpenAI as
	// output and are already counted inside completion_tokens — no separate
	// pricing field needed; they roll into the existing OutputPerMillion rate.
	usage := events.Usage{
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		TotalTokens:      apiResp.Usage.TotalTokens,
		CachedTokens:     apiResp.Usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens:  apiResp.Usage.CompletionTokensDetails.ReasoningTokens,
	}

	resp := events.LLMResponse{
		Model:   apiResp.Model,
		Usage:   usage,
		CostUSD: p.costForModel(apiResp.Model, usage),
	}

	if len(apiResp.Choices) > 0 {
		choice := apiResp.Choices[0]
		resp.FinishReason = choice.FinishReason
		if choice.Message.Content != nil {
			resp.Content = *choice.Message.Content
		}
		for _, tc := range choice.Message.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, events.ToolCallRequest{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	// Surface prediction acceptance counts when the request used the
	// prediction field. Lets callers tune their hit rates — accepted tokens
	// bill at the input rate, rejected at the output rate.
	if apiResp.Usage.CompletionTokensDetails.AcceptedPredictionTokens > 0 ||
		apiResp.Usage.CompletionTokensDetails.RejectedPredictionTokens > 0 {
		if resp.Metadata == nil {
			resp.Metadata = make(map[string]any)
		}
		resp.Metadata["prediction_acceptance"] = map[string]any{
			"accepted": apiResp.Usage.CompletionTokensDetails.AcceptedPredictionTokens,
			"rejected": apiResp.Usage.CompletionTokensDetails.RejectedPredictionTokens,
		}
	}

	return resp
}

// Streaming response handling.

type streamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Model   string              `json:"model"`
	Choices []streamChunkChoice `json:"choices"`
	Usage   *apiUsage           `json:"usage,omitempty"`
}

type streamChunkChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content,omitempty"`
	ToolCalls []apiToolCall `json:"tool_calls,omitempty"`
}

func (p *Plugin) handleStreamResponse(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var fullContent strings.Builder
	var toolCalls []events.ToolCallRequest
	// Track in-progress tool calls by index.
	toolCallBuilders := make(map[int]*events.ToolCallRequest)
	toolCallArgs := make(map[int]*strings.Builder)
	var usage apiUsage
	var model string
	var finishReason string
	turnID := ""
	chunkIndex := 0

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			p.logger.Warn("openai: failed to parse stream chunk", "error", err)
			continue
		}

		if turnID == "" {
			turnID = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}

		// Usage arrives in the final chunk when stream_options.include_usage is set.
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}

		// Text content delta.
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			text := *choice.Delta.Content
			fullContent.WriteString(text)
			_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{
				Content: text,
				Index:   chunkIndex,
				TurnID:  turnID,
			})
			chunkIndex++
		}

		// Tool call deltas.
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.ID // OpenAI uses index field, but ID is also set on first chunk
			// Use a numeric index from the tool call array position.
			tcIdx := 0
			// OpenAI streams tool calls with an index field. The ID and function
			// name arrive on the first chunk for each tool call; subsequent chunks
			// carry only the arguments delta. We key by the array index.
			if tc.Function.Name != "" {
				// First chunk for this tool call — allocate builder.
				tcIdx = len(toolCallBuilders)
				toolCallBuilders[tcIdx] = &events.ToolCallRequest{
					ID:   tc.ID,
					Name: tc.Function.Name,
				}
				toolCallArgs[tcIdx] = &strings.Builder{}
				if tc.Function.Arguments != "" {
					toolCallArgs[tcIdx].WriteString(tc.Function.Arguments)
				}
			} else {
				// Continuation chunk — find the matching builder.
				// Tool call chunks arrive in order, so the last one is the active one
				// unless we see a new name.
				tcIdx = len(toolCallBuilders) - 1
				if tcIdx >= 0 && toolCallArgs[tcIdx] != nil {
					toolCallArgs[tcIdx].WriteString(tc.Function.Arguments)
				}
			}
			_ = idx // suppress unused
		}
	}

	if err := scanner.Err(); err != nil {
		p.emitError(fmt.Errorf("openai: stream read error: %w", err))
	}

	// Finalize tool calls.
	for i := 0; i < len(toolCallBuilders); i++ {
		tc := toolCallBuilders[i]
		if args, ok := toolCallArgs[i]; ok {
			tc.Arguments = args.String()
		}
		toolCalls = append(toolCalls, *tc)

		_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{
			ToolCall: tc,
			Index:    chunkIndex,
			TurnID:   turnID,
		})
		chunkIndex++
	}

	// Build final usage. Reasoning tokens arrive in the final usage chunk
	// when stream_options.include_usage is set (already enabled above).
	// They're billed as output by OpenAI — already inside CompletionTokens.
	finalUsage := events.Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
	}

	// Emit stream end.
	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{
		TurnID:       turnID,
		FinishReason: finishReason,
		Usage:        finalUsage,
	})

	// Also emit the complete llm.response for downstream consumers.
	// Surface prediction acceptance the same way the sync path does.
	var streamMeta map[string]any
	if usage.CompletionTokensDetails.AcceptedPredictionTokens > 0 ||
		usage.CompletionTokensDetails.RejectedPredictionTokens > 0 {
		streamMeta = map[string]any{
			"prediction_acceptance": map[string]any{
				"accepted": usage.CompletionTokensDetails.AcceptedPredictionTokens,
				"rejected": usage.CompletionTokensDetails.RejectedPredictionTokens,
			},
		}
	}

	p.mu.Lock()
	mergedMeta := mergeMetadata(streamMeta, p.currentRequestMeta)
	p.mu.Unlock()

	_ = p.bus.Emit("llm.response", events.LLMResponse{
		Content:      fullContent.String(),
		ToolCalls:    toolCalls,
		Usage:        finalUsage,
		CostUSD:      p.costForModel(model, finalUsage),
		Model:        model,
		FinishReason: finishReason,
		Metadata:     mergedMeta,
	})
}

// parsePricingConfig merges embedded defaults with optional config overrides.
func parsePricingConfig(cfg map[string]any) map[string]modelPricing {
	merged := make(map[string]modelPricing, len(defaultPricing))
	for k, v := range defaultPricing {
		merged[k] = v
	}

	raw, ok := cfg["pricing"].(map[string]any)
	if !ok {
		return merged
	}

	for model, val := range raw {
		entry, ok := val.(map[string]any)
		if !ok {
			continue
		}
		p := merged[model]
		if v, ok := entry["input_per_million"].(float64); ok {
			p.InputPerMillion = v
		}
		if v, ok := entry["output_per_million"].(float64); ok {
			p.OutputPerMillion = v
		}
		if v, ok := entry["cache_read_per_million"].(float64); ok {
			p.CacheReadPerMillion = v
		}
		merged[model] = p
	}

	return merged
}

// costForModel returns the USD cost for a response from the given model.
func (p *Plugin) costForModel(model string, usage events.Usage) float64 {
	if rates, ok := p.pricing[model]; ok {
		return calculateCost(usage, rates)
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
		p.logger.Error("failed to write debug log", "file", filename, "error", err)
	}
}

func (p *Plugin) emitError(err error) {
	p.emitErrorInfo(events.ErrorInfo{
		Source: pluginID,
		Err:    err,
		Fatal:  false,
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
