package batch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// Anthropic Message Batches endpoints + headers.
//
// The Messages Batches API ships behind a beta gate. We send the beta header
// on every batch request so the gate stays on regardless of deployment.
const (
	anthropicBatchBaseURL    = "https://api.anthropic.com/v1/messages/batches"
	anthropicAPIVersion      = "2023-06-01"
	anthropicBatchBetaHeader = "message-batches-2024-09-24"
)

// submitAnthropic POSTs a new Message Batches job and returns the batch id.
//
// Body shape:
//
//	{"requests": [
//	  {"custom_id":"...","params":{...messages-api-body...}},
//	  ...
//	]}
//
// The per-request "params" payload is constructed by buildAnthropicMessageBody —
// a deliberately minimal text-only adapter (no thinking, no caching, no
// multimodal). The full provider plugin's buildRequestBody isn't reused here
// because it's tightly coupled to Plugin state (auth modes, file uploads,
// metadata side-effects). Surfacing those features in batched mode is a
// follow-up; calling them out here so it's not a silent gap.
func (p *Plugin) submitAnthropic(ctx context.Context, requests []events.BatchRequest) (string, error) {
	if p.anthropicAPIKey == "" {
		return "", fmt.Errorf("batch: anthropic api key not configured")
	}
	if len(requests) == 0 {
		return "", fmt.Errorf("batch: anthropic requires at least one request")
	}

	type wireRequest struct {
		CustomID string         `json:"custom_id"`
		Params   map[string]any `json:"params"`
	}
	wire := make([]wireRequest, 0, len(requests))
	for _, r := range requests {
		params, err := buildAnthropicMessageBody(r.Request, p.defaultMaxTokens)
		if err != nil {
			return "", fmt.Errorf("batch: anthropic request %q: %w", r.CustomID, err)
		}
		wire = append(wire, wireRequest{CustomID: r.CustomID, Params: params})
	}

	body, err := json.Marshal(map[string]any{"requests": wire})
	if err != nil {
		return "", fmt.Errorf("batch: anthropic marshal submit body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.anthropicURL(""), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("batch: anthropic build submit request: %w", err)
	}
	p.applyAnthropicHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("batch: anthropic submit HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("batch: anthropic submit returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("batch: anthropic decode submit response: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("batch: anthropic submit response missing id: %s", string(respBody))
	}
	return parsed.ID, nil
}

// statusAnthropic fetches the current status of a Messages batch and maps the
// provider's processing_status to the canonical status vocabulary.
func (p *Plugin) statusAnthropic(ctx context.Context, batchID string) (string, events.BatchCounts, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.anthropicURL("/"+batchID), nil)
	if err != nil {
		return "", events.BatchCounts{}, fmt.Errorf("batch: anthropic build status request: %w", err)
	}
	p.applyAnthropicHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", events.BatchCounts{}, fmt.Errorf("batch: anthropic status HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", events.BatchCounts{}, fmt.Errorf("batch: anthropic status returned %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID               string `json:"id"`
		ProcessingStatus string `json:"processing_status"`
		RequestCounts    struct {
			Processing int `json:"processing"`
			Succeeded  int `json:"succeeded"`
			Errored    int `json:"errored"`
			Canceled   int `json:"canceled"`
			Expired    int `json:"expired"`
		} `json:"request_counts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", events.BatchCounts{}, fmt.Errorf("batch: anthropic decode status: %w", err)
	}

	counts := events.BatchCounts{
		Total: parsed.RequestCounts.Processing + parsed.RequestCounts.Succeeded +
			parsed.RequestCounts.Errored + parsed.RequestCounts.Canceled +
			parsed.RequestCounts.Expired,
		Completed: parsed.RequestCounts.Succeeded,
		Failed:    parsed.RequestCounts.Errored + parsed.RequestCounts.Canceled + parsed.RequestCounts.Expired,
	}

	// Anthropic uses "ended" for the terminal state regardless of outcome —
	// flip it to "completed" when at least one request succeeded, "failed"
	// when none did. "in_progress" stays as-is.
	canonical := parsed.ProcessingStatus
	switch parsed.ProcessingStatus {
	case "ended":
		if parsed.RequestCounts.Succeeded > 0 {
			canonical = statusCompleted
		} else {
			canonical = statusFailed
		}
	case "in_progress":
		canonical = statusInProgress
	case "canceling", "canceled", "cancelled":
		canonical = statusCancelled
	}
	return canonical, counts, nil
}

// resultsAnthropic downloads the JSONL results file for a batch and decodes
// each line into a BatchResult.
//
// Anthropic's results endpoint redirects to a presigned URL; net/http follows
// 3xx by default. We don't reauthenticate the redirect — the presigned URL
// carries its own credentials.
func (p *Plugin) resultsAnthropic(ctx context.Context, batchID string) ([]events.BatchResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.anthropicURL("/"+batchID+"/results"), nil)
	if err != nil {
		return nil, fmt.Errorf("batch: anthropic build results request: %w", err)
	}
	p.applyAnthropicHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch: anthropic results HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("batch: anthropic results returned %d: %s", resp.StatusCode, string(body))
	}

	out := make([]events.BatchResult, 0, 16)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024*16) // up to 16 MiB per line
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		res, err := decodeAnthropicResultLine([]byte(line))
		if err != nil {
			out = append(out, events.BatchResult{Error: fmt.Sprintf("decode line: %v", err)})
			continue
		}
		out = append(out, res)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("batch: anthropic results read: %w", err)
	}
	return out, nil
}

// decodeAnthropicResultLine parses one JSONL line from Anthropic's results
// stream. Each line carries a custom_id and a result envelope of one of:
//
//	{"type":"succeeded","message":{...messages-api-response...}}
//	{"type":"errored","error":{"type":"...","message":"..."}}
//	{"type":"canceled"} | {"type":"expired"}
func decodeAnthropicResultLine(line []byte) (events.BatchResult, error) {
	var raw struct {
		CustomID string `json:"custom_id"`
		Result   struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message,omitempty"`
			Error   struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return events.BatchResult{}, err
	}

	br := events.BatchResult{CustomID: raw.CustomID}
	switch raw.Result.Type {
	case "succeeded":
		resp, err := decodeAnthropicMessage(raw.Result.Message)
		if err != nil {
			br.Error = fmt.Sprintf("decode message: %v", err)
			return br, nil
		}
		br.Response = resp
	case "errored":
		if raw.Result.Error.Type != "" || raw.Result.Error.Message != "" {
			br.Error = fmt.Sprintf("%s: %s", raw.Result.Error.Type, raw.Result.Error.Message)
		} else {
			br.Error = "errored"
		}
	case "canceled", "cancelled":
		br.Error = "canceled"
	case "expired":
		br.Error = "expired"
	default:
		br.Error = fmt.Sprintf("unknown result type: %s", raw.Result.Type)
	}
	return br, nil
}

// decodeAnthropicMessage maps an Anthropic Messages API response object onto
// our LLMResponse. Mirrors plugins/providers/anthropic/plugin.go's
// convertAPIResponse but kept local to avoid pulling in that plugin's
// internal types here.
func decodeAnthropicMessage(raw json.RawMessage) (*events.LLMResponse, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty message payload")
	}
	var msg struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			ID    string          `json:"id,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}

	var content strings.Builder
	var toolCalls []events.ToolCallRequest
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, events.ToolCallRequest{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}

	return &events.LLMResponse{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		Model:        msg.Model,
		FinishReason: msg.StopReason,
		Usage: events.Usage{
			PromptTokens:     msg.Usage.InputTokens,
			CompletionTokens: msg.Usage.OutputTokens,
			CachedTokens:     msg.Usage.CacheReadInputTokens,
			CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
			TotalTokens:      msg.Usage.InputTokens + msg.Usage.OutputTokens,
		},
	}, nil
}

// buildAnthropicMessageBody is the minimal LLMRequest -> Messages-API body
// adapter used by the batch coordinator. It supports text-only messages
// (system + user/assistant turns + tool calls/results) and basic tool
// definitions; multimodal parts, extended thinking, prompt caching, citations,
// structured outputs, and Bedrock/Vertex auth are intentionally OUT OF SCOPE
// for v1. The provider plugin's buildRequestBody owns those features and is
// not exported, so reusing it here would require a refactor.
//
// defaultMaxTokens is the fallback applied when a request didn't pin a value
// itself (Anthropic requires max_tokens; sending zero is rejected).
func buildAnthropicMessageBody(req events.LLMRequest, defaultMaxTokens int) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("model is required for anthropic batch requests")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	body := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	var systemPrompt string
	apiMessages := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			if systemPrompt == "" {
				systemPrompt = msg.Content
			} else {
				systemPrompt = systemPrompt + "\n\n" + msg.Content
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				content := make([]map[string]any, 0, len(msg.ToolCalls)+1)
				if msg.Content != "" {
					content = append(content, map[string]any{"type": "text", "text": msg.Content})
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
				apiMessages = append(apiMessages, map[string]any{"role": "assistant", "content": content})
				continue
			}
			apiMessages = append(apiMessages, map[string]any{"role": "assistant", "content": msg.Content})
		case "tool":
			apiMessages = append(apiMessages, map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": msg.Content},
				},
			})
		case "user":
			apiMessages = append(apiMessages, map[string]any{"role": "user", "content": msg.Content})
		default:
			apiMessages = append(apiMessages, map[string]any{"role": msg.Role, "content": msg.Content})
		}
	}
	if systemPrompt != "" {
		body["system"] = systemPrompt
	}
	body["messages"] = apiMessages

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = tools
	}

	return body, nil
}

// anthropicURL composes the absolute URL for an endpoint suffix. Tests
// override anthropicBaseURL to point at an httptest server.
func (p *Plugin) anthropicURL(suffix string) string {
	base := p.anthropicBaseURL
	if base == "" {
		base = anthropicBatchBaseURL
	}
	return base + suffix
}

// applyAnthropicHeaders attaches the API key, version, and beta gate to req.
// Centralized so submit/status/results stay in lockstep.
func (p *Plugin) applyAnthropicHeaders(req *http.Request) {
	req.Header.Set("x-api-key", p.anthropicAPIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("anthropic-beta", anthropicBatchBetaHeader)
}
