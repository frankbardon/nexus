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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.llm.anthropic"
	pluginName = "Anthropic LLM Provider"
	version    = "0.1.0"
	apiURL     = "https://api.anthropic.com/v1/messages"
)

// modelPricing holds per-model token rates in USD per million tokens.
//
// Cache rates are optional: when zero, calculateCost falls back to derived
// multiples of InputPerMillion (read = 0.10×, write_5m = 1.25×, write_1h = 2.0×)
// per Anthropic's published cache pricing.
type modelPricing struct {
	InputPerMillion        float64
	OutputPerMillion       float64
	CacheReadPerMillion    float64 // 0 → InputPerMillion * 0.10
	CacheWrite5mPerMillion float64 // 0 → InputPerMillion * 1.25
	CacheWrite1hPerMillion float64 // 0 → InputPerMillion * 2.0
}

// defaultPricing is the embedded fallback pricing table. Updated via patch
// releases when providers change rates. Config override always wins.
var defaultPricing = map[string]modelPricing{
	"claude-sonnet-4-6-20250514": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-sonnet-4-5-20250514": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5-20251001":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	"claude-opus-4-6-20250602":   {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-3-5-sonnet-20241022": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-3-5-haiku-20241022":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	"claude-3-opus-20240229":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
}

// calculateCost computes the USD cost for a single LLM call.
//
// Anthropic's `input_tokens` already excludes cache-creation and cache-read
// portions, so usage.PromptTokens is the cache-miss (plain input) count and
// CachedTokens / CacheWriteTokens are billed separately at their own rates.
//
// We currently treat all CacheWriteTokens as 5-minute-TTL writes; per-request
// TTL selection (cf. plan 01) will route writes to the 1h rate when needed.
func calculateCost(usage events.Usage, rates modelPricing) float64 {
	cacheReadRate := rates.CacheReadPerMillion
	if cacheReadRate == 0 {
		cacheReadRate = rates.InputPerMillion * 0.10
	}
	cacheWriteRate := rates.CacheWrite5mPerMillion
	if cacheWriteRate == 0 {
		cacheWriteRate = rates.InputPerMillion * 1.25
	}
	return float64(usage.PromptTokens)/1_000_000*rates.InputPerMillion +
		float64(usage.CacheWriteTokens)/1_000_000*cacheWriteRate +
		float64(usage.CachedTokens)/1_000_000*cacheReadRate +
		float64(usage.CompletionTokens)/1_000_000*rates.OutputPerMillion
}

// Plugin implements the Anthropic LLM provider.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	models  *engine.ModelRegistry
	session *engine.SessionWorkspace

	apiKey  string
	client  *http.Client
	prompts *engine.PromptRegistry
	unsubs  []func()
	debug   bool
	retry   retryConfig
	cache   cacheConfig
	pricing map[string]modelPricing // merged: config overrides + embedded defaults

	mu                 sync.Mutex
	currentRequestMeta map[string]any
	cancelFunc         context.CancelFunc // cancels the in-flight HTTP request
	requestSeq         int                // monotonic counter for debug log filenames
}

// New creates a new Anthropic provider plugin.
func New() engine.Plugin {
	return &Plugin{}
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

	if debug, ok := ctx.Config["debug"].(bool); ok {
		p.debug = debug
	}

	// Read API key: direct config value takes priority over env var.
	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		apiKeyEnv, _ := ctx.Config["api_key_env"].(string)
		if apiKeyEnv == "" {
			apiKeyEnv = "ANTHROPIC_API_KEY"
		}
		p.apiKey = os.Getenv(apiKeyEnv)
	}
	if p.apiKey == "" {
		return fmt.Errorf("anthropic: no API key configured (set api_key in config or %s env var)", "ANTHROPIC_API_KEY")
	}

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

	p.logger.Debug("resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)

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

	makeReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", apiURL, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-api-key", p.apiKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("content-type", "application/json")
		// 1h TTL caching is gated behind a beta header. Plan 06 will introduce
		// a metadata-driven multi-header builder; for now this is the only
		// beta flag the plugin emits, so a single Set is fine.
		if p.cache.Enabled && p.cache.TTL == "1h" {
			httpReq.Header.Set("anthropic-beta", "extended-cache-ttl-2025-04-11")
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
		p.emitErrorInfo(events.ErrorInfo{
			Err:              fmt.Errorf("anthropic: HTTP request failed: %w", err),
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
			Err:         fmt.Errorf("anthropic: API returned status %d: %s", resp.StatusCode, string(respBody)),
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

	// Structured output simulation via tool-use-as-schema.
	// Inject synthetic tool and force tool choice to it.
	if rf := req.ResponseFormat; rf != nil && rf.Type == "json_schema" && rf.Schema != nil {
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
	} else {
		// Map tool choice to Anthropic API format (only when not overridden by structured output).
		if tc := resolveToolChoice(req.ToolChoice, filteredTools); tc != nil {
			body["tool_choice"] = tc
		}
	}

	// Mark cacheable prefix segments (system, last tool, leading user msgs) per
	// configured policy. No-op when caching is disabled.
	applyCacheControl(body, p.cache, p.logger)

	return body
}

// convertMessage converts an events.Message to the Anthropic API format.
func (p *Plugin) convertMessage(msg events.Message) map[string]any {
	switch msg.Role {
	case "assistant":
		if len(msg.ToolCalls) > 0 {
			var content []map[string]any
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
		return map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     msg.Content,
				},
			},
		}

	case "user":
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

	p.mu.Lock()
	resp.Metadata = p.currentRequestMeta
	p.mu.Unlock()

	if err := p.bus.Emit("llm.response", resp); err != nil {
		p.logger.Error("failed to emit llm.response", "error", err)
	}
}

func (p *Plugin) convertAPIResponse(apiResp apiResponse) events.LLMResponse {
	var content strings.Builder
	var toolCalls []events.ToolCallRequest

	for _, block := range apiResp.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
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

	return events.LLMResponse{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		CostUSD:      p.costForModel(apiResp.Model, usage),
		Model:        apiResp.Model,
		FinishReason: apiResp.StopReason,
	}
}

// SSE streaming response handling.

type sseEvent struct {
	Event string
	Data  string
}

func (p *Plugin) handleStreamResponse(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var currentEvent sseEvent
	var fullContent strings.Builder
	var toolCalls []events.ToolCallRequest
	var currentToolCall *events.ToolCallRequest
	var currentToolInput strings.Builder
	var usage apiUsage
	var model string
	var finishReason string
	turnID := ""
	chunkIndex := 0

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line means end of SSE event; process it.
			if currentEvent.Event != "" {
				p.processSSEEvent(currentEvent, &fullContent, &toolCalls, &currentToolCall,
					&currentToolInput, &usage, &model, &finishReason, &turnID, &chunkIndex)
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
		p.processSSEEvent(currentEvent, &fullContent, &toolCalls, &currentToolCall,
			&currentToolInput, &usage, &model, &finishReason, &turnID, &chunkIndex)
	}

	if err := scanner.Err(); err != nil {
		p.emitError(fmt.Errorf("anthropic: stream read error: %w", err))
	}

	// Build final usage.
	finalUsage := events.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		CachedTokens:     usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
		TotalTokens: usage.InputTokens + usage.OutputTokens +
			usage.CacheReadInputTokens + usage.CacheCreationInputTokens,
	}

	// Emit stream end.
	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{
		TurnID:       turnID,
		FinishReason: finishReason,
		Usage:        finalUsage,
	})

	// Also emit the complete llm.response for downstream consumers.
	p.mu.Lock()
	meta := p.currentRequestMeta
	p.mu.Unlock()

	_ = p.bus.Emit("llm.response", events.LLMResponse{
		Content:      fullContent.String(),
		ToolCalls:    toolCalls,
		Usage:        finalUsage,
		CostUSD:      p.costForModel(model, finalUsage),
		Model:        model,
		FinishReason: finishReason,
		Metadata:     meta,
	})
}

func (p *Plugin) processSSEEvent(
	sse sseEvent,
	fullContent *strings.Builder,
	toolCalls *[]events.ToolCallRequest,
	currentToolCall **events.ToolCallRequest,
	currentToolInput *strings.Builder,
	usage *apiUsage,
	model *string,
	finishReason *string,
	turnID *string,
	chunkIndex *int,
) {
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
			*turnID = data.Message.ID
			*model = data.Message.Model
			usage.InputTokens = data.Message.Usage.InputTokens
			usage.CacheCreationInputTokens = data.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = data.Message.Usage.CacheReadInputTokens
		}

	case "content_block_start":
		var data struct {
			Index        int             `json:"index"`
			ContentBlock apiContentBlock `json:"content_block"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			if data.ContentBlock.Type == "tool_use" {
				*currentToolCall = &events.ToolCallRequest{
					ID:   data.ContentBlock.ID,
					Name: data.ContentBlock.Name,
				}
				currentToolInput.Reset()
			}
		}

	case "content_block_delta":
		var data struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			switch data.Delta.Type {
			case "text_delta":
				fullContent.WriteString(data.Delta.Text)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{
					Content: data.Delta.Text,
					Index:   *chunkIndex,
					TurnID:  *turnID,
				})
				*chunkIndex++

			case "input_json_delta":
				if *currentToolCall != nil {
					currentToolInput.WriteString(data.Delta.PartialJSON)
					// Stream structured output tool input as content chunks.
					if (*currentToolCall).Name == "_structured_output" {
						_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{
							Content: data.Delta.PartialJSON,
							Index:   *chunkIndex,
							TurnID:  *turnID,
						})
						*chunkIndex++
					}
				}
			}
		}

	case "content_block_stop":
		if *currentToolCall != nil {
			(*currentToolCall).Arguments = currentToolInput.String()

			if (*currentToolCall).Name == "_structured_output" {
				// Unwrap synthetic tool — accumulate into content, not tool calls.
				fullContent.WriteString(currentToolInput.String())
			} else {
				*toolCalls = append(*toolCalls, **currentToolCall)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{
					ToolCall: *currentToolCall,
					Index:    *chunkIndex,
					TurnID:   *turnID,
				})
				*chunkIndex++
			}

			*currentToolCall = nil
			currentToolInput.Reset()
		}

	case "message_delta":
		var data struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage apiUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(sse.Data), &data) == nil {
			*finishReason = data.Delta.StopReason
			if data.Usage.OutputTokens > 0 {
				usage.OutputTokens = data.Usage.OutputTokens
			}
			// message_delta may carry an updated cache snapshot (Anthropic
			// sometimes finalizes counts here). Prefer the larger value so
			// we don't regress message_start totals.
			if data.Usage.CacheCreationInputTokens > usage.CacheCreationInputTokens {
				usage.CacheCreationInputTokens = data.Usage.CacheCreationInputTokens
			}
			if data.Usage.CacheReadInputTokens > usage.CacheReadInputTokens {
				usage.CacheReadInputTokens = data.Usage.CacheReadInputTokens
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

// parsePricingConfig merges embedded defaults with optional config overrides.
// Config format:
//
//	pricing:
//	  claude-sonnet-4-6-20250514:
//	    input_per_million: 3.0
//	    output_per_million: 15.0
//	    cache_read_per_million: 0.30
//	    cache_write_5m_per_million: 3.75
//	    cache_write_1h_per_million: 6.0
//
// Cache rates are optional; calculateCost derives them from input rate when
// unset (read = 0.10×, write 5m = 1.25×, write 1h = 2.0×).
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
		if v, ok := entry["cache_write_5m_per_million"].(float64); ok {
			p.CacheWrite5mPerMillion = v
		}
		if v, ok := entry["cache_write_1h_per_million"].(float64); ok {
			p.CacheWrite1hPerMillion = v
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
