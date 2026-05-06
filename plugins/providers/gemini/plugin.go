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
	pricing *pricing.Table

	thinking      thinkingConfig
	codeExecution bool
	cache         *cacheState

	mu                 sync.Mutex
	currentRequestMeta map[string]any
	currentRequestTags map[string]string // copied to llm.response for cost attribution
	cancelFunc         context.CancelFunc
	requestSeq         int
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

	p.client = &http.Client{Timeout: 5 * time.Minute}

	p.pricing = parsePricingConfig(ctx.Config)
	p.retry = parseRetryConfig(ctx.Config)
	p.thinking = parseThinkingConfig(ctx.Config)

	if v, ok := ctx.Config["code_execution"].(bool); ok {
		p.codeExecution = v
	}

	p.cache = newCacheState(ctx.Config, p.logger)

	if p.retry.Enabled {
		p.logger.Info("retry enabled",
			"max_retries", p.retry.MaxRetries,
			"backoff", string(p.retry.Backoff),
			"initial_delay", p.retry.InitialDelay,
			"max_delay", p.retry.MaxDelay,
		)
	}
	if p.thinking.Enabled {
		p.logger.Info("thinking enabled",
			"budget_tokens", p.thinking.BudgetTokens,
			"include_thoughts", p.thinking.IncludeThoughts,
		)
	}
	if p.codeExecution {
		p.logger.Info("code_execution tool enabled")
	}
	if p.cache.enabled {
		p.logger.Info("prompt caching enabled",
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
		"llm.response",
		"llm.stream.chunk",
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
	cancel := p.cancelFunc
	p.cancelFunc = nil
	p.mu.Unlock()

	if cancel != nil {
		p.logger.Info("cancelling in-flight LLM request")
		cancel()
	}
}

func (p *Plugin) handleRequest(req events.LLMRequest) {
	if target, ok := req.Metadata["_target_provider"].(string); ok && target != pluginID {
		return
	}

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

	model := req.Model
	maxTokens := req.MaxTokens

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

	if model == "" && p.models != nil {
		def := p.models.Default()
		if def.Provider == "" || def.Provider == pluginID {
			model = def.Model
			if maxTokens == 0 {
				maxTokens = def.MaxTokens
			}
		}
	}

	if model == "" {
		p.emitError(fmt.Errorf("gemini: no model resolved for role %q", req.Role))
		return
	}

	// max_tokens may still be 0 — common when a router (idea 09) rewrote
	// req.Model to a concrete id without touching MaxTokens, so the
	// model-resolution branches above were skipped.
	if maxTokens == 0 && req.Role != "" {
		if cfg, ok := p.models.Resolve(req.Role); ok && cfg.MaxTokens > 0 {
			maxTokens = cfg.MaxTokens
		}
	}
	if maxTokens == 0 {
		if def := p.models.Default(); def.MaxTokens > 0 {
			maxTokens = def.MaxTokens
		}
	}
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	p.logger.Debug("resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)

	body, err := p.buildRequestBody(model, maxTokens, req)
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
	p.mu.Lock()
	p.cancelFunc = cancel
	p.mu.Unlock()

	makeReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if err := p.auth.applyAuth(reqCtx, httpReq, p.client); err != nil {
			return nil, fmt.Errorf("apply auth: %w", err)
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
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("gemini: HTTP request failed: %w", err),
			Retryable:        true,
			RetriesExhausted: true,
			RequestMeta:      req.Metadata,
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		p.emitErrorInfo(events.ErrorInfo{SchemaVersion: events.ErrorInfoVersion, Err: fmt.Errorf("gemini: API returned status %d: %s", resp.StatusCode, string(respBody)),
			Retryable:   false,
			RequestMeta: req.Metadata,
		})
		return
	}

	meta := req.Metadata
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "text" {
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

// buildRequestBody constructs a Gemini generateContent request body.
func (p *Plugin) buildRequestBody(model string, maxTokens int, req events.LLMRequest) (map[string]any, error) {
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

	// Thinking config.
	if p.thinking.Enabled {
		tc := map[string]any{}
		if p.thinking.IncludeThoughts {
			tc["includeThoughts"] = true
		}
		if p.thinking.BudgetTokens > 0 {
			tc["thinkingBudget"] = p.thinking.BudgetTokens
		}
		if len(tc) > 0 {
			gen["thinkingConfig"] = tc
		}
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
	// when eligible.
	if p.cache.enabled {
		if cachedName := p.cache.lookup(model, systemPrompt, filteredTools, contents); cachedName != "" {
			body["cachedContent"] = cachedName
			// Caller's contents already includes only the trailing delta; the
			// cache plugin stripped what's covered by the cache. See cache.go.
		}
	}

	return body, nil
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
			for _, tc := range msg.ToolCalls {
				var args any
				if tc.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
						args = map[string]any{}
					}
				} else {
					args = map[string]any{}
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": tc.Name,
						"args": args,
					},
				})
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
			// Gemini expects response.content to be a structured object. Wrap
			// raw text in {output: ...} for round-trip safety.
			var responseObj any
			if err := json.Unmarshal([]byte(msg.Content), &responseObj); err != nil {
				responseObj = map[string]any{"output": msg.Content}
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
// doesn't accept (e.g. additionalProperties, $schema, $id). Walks recursively.
func sanitizeSchemaForGemini(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		switch k {
		case "$schema", "$id", "$ref", "additionalProperties", "definitions", "$defs":
			continue
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
	Text                string             `json:"text,omitempty"`
	Thought             bool               `json:"thought,omitempty"`
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
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

func (p *Plugin) handleSyncResponse(body io.Reader) {
	var apiResp apiResponse
	if err := json.NewDecoder(body).Decode(&apiResp); err != nil {
		p.emitError(fmt.Errorf("gemini: decode response: %w", err))
		return
	}

	resp := p.convertAPIResponse(apiResp, generateTurnID())

	p.mu.Lock()
	resp.Metadata = p.currentRequestMeta
	resp.Tags = p.currentRequestTags
	p.mu.Unlock()

	if err := p.bus.Emit("llm.response", resp); err != nil {
		p.logger.Error("failed to emit llm.response", "error", err)
	}
}

// convertAPIResponse normalizes a Gemini response into events.LLMResponse.
// Thinking parts are emitted as thinking.step events (when turnID is non-empty)
// or skipped from Content. Code execution parts are dual-emitted as content
// and tool.invoke / tool.result events.
func (p *Plugin) convertAPIResponse(apiResp apiResponse, turnID string) events.LLMResponse {
	var content strings.Builder
	var toolCalls []events.ToolCallRequest
	var finishReason string
	model := apiResp.ModelVersion

	if len(apiResp.Candidates) > 0 {
		cand := apiResp.Candidates[0]
		finishReason = cand.FinishReason

		toolCallSeq := 0
		for _, part := range cand.Content.Parts {
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
	}

	return events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: content.String(),
		ToolCalls:    toolCalls,
		Usage:        usage,
		CostUSD:      p.costForModel(model, usage),
		Model:        model,
		FinishReason: finishReason,
	}
}

// generateTurnID returns a synthetic per-stream identifier. Gemini's API does
// not assign one and the engine relies on a stable TurnID to associate
// streaming chunks with the bubble they belong to.
func generateTurnID() string {
	return fmt.Sprintf("gemini_%d", time.Now().UnixNano())
}

// SSE streaming.

func (p *Plugin) handleStreamResponse(body io.Reader) {
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
	chunkIndex := 0
	toolCallSeq := 0

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
			switch {
			case part.Thought && part.Text != "":
				_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: turnID,
					Source:    pluginID,
					Content:   part.Text,
					Phase:     "reasoning",
					Timestamp: time.Now(),
				})

			case part.Text != "":
				fullContent.WriteString(part.Text)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: part.Text,
					Index:  chunkIndex,
					TurnID: turnID,
				})
				chunkIndex++

			case part.FunctionCall != nil:
				args, _ := json.Marshal(part.FunctionCall.Args)
				tc := events.ToolCallRequest{
					ID:        fmt.Sprintf("call_%d_%s", toolCallSeq, part.FunctionCall.Name),
					Name:      part.FunctionCall.Name,
					Arguments: string(args),
				}
				toolCalls = append(toolCalls, tc)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, ToolCall: &tc,
					Index:  chunkIndex,
					TurnID: turnID,
				})
				chunkIndex++
				toolCallSeq++

			case part.ExecutableCode != nil:
				code := part.ExecutableCode.Code
				lang := part.ExecutableCode.Language
				snippet := fmt.Sprintf("\n```%s\n%s\n```\n", strings.ToLower(lang), code)
				fullContent.WriteString(snippet)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: snippet,
					Index:  chunkIndex,
					TurnID: turnID,
				})
				chunkIndex++
				_ = p.bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "_gemini_code_execution",
					Arguments: map[string]any{"language": lang, "code": code},
				})

			case part.CodeExecutionResult != nil:
				out := part.CodeExecutionResult.Output
				outcome := part.CodeExecutionResult.Outcome
				snippet := fmt.Sprintf("\n```output\n%s\n```\n", out)
				fullContent.WriteString(snippet)
				_ = p.bus.Emit("llm.stream.chunk", events.StreamChunk{SchemaVersion: events.StreamChunkVersion, Content: snippet,
					Index:  chunkIndex,
					TurnID: turnID,
				})
				chunkIndex++
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

	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{SchemaVersion: events.StreamEndVersion, TurnID: turnID,
		FinishReason: finishReason,
		Usage:        finalUsage,
	})

	p.mu.Lock()
	meta := p.currentRequestMeta
	tags := p.currentRequestTags
	p.mu.Unlock()

	_ = p.bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: fullContent.String(),
		ToolCalls:    toolCalls,
		Usage:        finalUsage,
		CostUSD:      p.costForModel(model, finalUsage),
		Model:        model,
		FinishReason: finishReason,
		Metadata:     meta,
		Tags:         tags,
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
