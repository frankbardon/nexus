package batch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// OpenAI Batch API endpoints.
//
// Submit is a two-step dance: upload a JSONL request file via /v1/files with
// purpose=batch, then create a batch job pointing at the resulting file id.
// Results are downloaded by fetching the output file's content.
const (
	openaiFilesURL          = "https://api.openai.com/v1/files"
	openaiBatchesURL        = "https://api.openai.com/v1/batches"
	openaiCompletionWindow  = "24h"
	openaiBatchEndpointPath = "/v1/chat/completions"
)

// submitOpenAI uploads a JSONL request file then creates a batch job.
//
// JSONL line shape:
//
//	{"custom_id":"...","method":"POST","url":"/v1/chat/completions","body":{...}}
//
// Each line's "body" is built by buildOpenAIChatBody — a minimal text-only
// adapter mirroring the Anthropic counterpart. The full provider plugin's
// buildRequestBody isn't reused (private + tightly coupled to plugin state).
func (p *Plugin) submitOpenAI(ctx context.Context, requests []events.BatchRequest) (string, error) {
	if p.openaiAPIKey == "" {
		return "", fmt.Errorf("batch: openai api key not configured")
	}
	if len(requests) == 0 {
		return "", fmt.Errorf("batch: openai requires at least one request")
	}

	// Step 1: build JSONL bytes.
	var jsonl bytes.Buffer
	enc := json.NewEncoder(&jsonl)
	for _, r := range requests {
		body, err := buildOpenAIChatBody(r.Request, p.defaultMaxTokens)
		if err != nil {
			return "", fmt.Errorf("batch: openai request %q: %w", r.CustomID, err)
		}
		line := map[string]any{
			"custom_id": r.CustomID,
			"method":    "POST",
			"url":       openaiBatchEndpointPath,
			"body":      body,
		}
		if err := enc.Encode(line); err != nil {
			return "", fmt.Errorf("batch: openai encode jsonl line: %w", err)
		}
	}

	// Step 2: upload as a "batch"-purpose file.
	fileID, err := p.uploadOpenAIBatchFile(ctx, jsonl.Bytes())
	if err != nil {
		return "", err
	}

	// Step 3: create the batch job.
	body, err := json.Marshal(map[string]any{
		"input_file_id":     fileID,
		"endpoint":          openaiBatchEndpointPath,
		"completion_window": openaiCompletionWindow,
	})
	if err != nil {
		return "", fmt.Errorf("batch: openai marshal create-batch body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.openaiBatchesURL(""), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("batch: openai build create-batch request: %w", err)
	}
	p.applyOpenAIHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("batch: openai create-batch HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("batch: openai create-batch returned %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("batch: openai decode create-batch response: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("batch: openai create-batch missing id: %s", string(respBody))
	}
	return parsed.ID, nil
}

// uploadOpenAIBatchFile multipart-POSTs the JSONL bytes to /v1/files with
// purpose=batch and returns the file id. Tests inject an alternate base URL
// via Plugin.openaiFilesBaseURL.
func (p *Plugin) uploadOpenAIBatchFile(ctx context.Context, jsonl []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("purpose", "batch"); err != nil {
		return "", fmt.Errorf("batch: openai write multipart purpose: %w", err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="batch.jsonl"`)
	header.Set("Content-Type", "application/jsonl")
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("batch: openai create multipart part: %w", err)
	}
	if _, err := part.Write(jsonl); err != nil {
		return "", fmt.Errorf("batch: openai write multipart payload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("batch: openai close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.openaiFilesURL(""), &buf)
	if err != nil {
		return "", fmt.Errorf("batch: openai build files request: %w", err)
	}
	p.applyOpenAIHeaders(req)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("batch: openai files HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("batch: openai files returned %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("batch: openai decode files response: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("batch: openai files response missing id: %s", string(respBody))
	}
	return parsed.ID, nil
}

// statusOpenAI fetches the current state of an OpenAI batch and maps OpenAI's
// status vocabulary to ours. Returns the (possibly empty) output_file_id via
// the metadata side-channel — fetched only once at completion time, so we
// expose it through a sentinel field on activeBatch rather than the public
// BatchStatus payload.
func (p *Plugin) statusOpenAI(ctx context.Context, batchID string) (status string, counts events.BatchCounts, outputFileID string, errorFileID string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.openaiBatchesURL("/"+batchID), nil)
	if err != nil {
		return "", events.BatchCounts{}, "", "", fmt.Errorf("batch: openai build status request: %w", err)
	}
	p.applyOpenAIHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", events.BatchCounts{}, "", "", fmt.Errorf("batch: openai status HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", events.BatchCounts{}, "", "", fmt.Errorf("batch: openai status returned %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		OutputFileID  string `json:"output_file_id"`
		ErrorFileID   string `json:"error_file_id"`
		RequestCounts struct {
			Total     int `json:"total"`
			Completed int `json:"completed"`
			Failed    int `json:"failed"`
		} `json:"request_counts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", events.BatchCounts{}, "", "", fmt.Errorf("batch: openai decode status: %w", err)
	}

	canonical := parsed.Status
	switch parsed.Status {
	case "validating", "in_progress", "finalizing":
		canonical = statusInProgress
	case "completed":
		canonical = statusCompleted
	case "failed", "expired":
		canonical = statusFailed
	case "cancelling", "cancelled", "canceling", "canceled":
		canonical = statusCancelled
	}
	counts = events.BatchCounts{
		Total:     parsed.RequestCounts.Total,
		Completed: parsed.RequestCounts.Completed,
		Failed:    parsed.RequestCounts.Failed,
	}
	return canonical, counts, parsed.OutputFileID, parsed.ErrorFileID, nil
}

// resultsOpenAI downloads the JSONL output file for a completed batch and
// decodes each line into a BatchResult.
func (p *Plugin) resultsOpenAI(ctx context.Context, outputFileID string) ([]events.BatchResult, error) {
	if outputFileID == "" {
		return nil, fmt.Errorf("batch: openai missing output_file_id")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", p.openaiFilesURL("/"+outputFileID+"/content"), nil)
	if err != nil {
		return nil, fmt.Errorf("batch: openai build results request: %w", err)
	}
	p.applyOpenAIHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch: openai results HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("batch: openai results returned %d: %s", resp.StatusCode, string(body))
	}

	out := make([]events.BatchResult, 0, 16)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024*16) // up to 16 MiB per line
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		res, err := decodeOpenAIResultLine([]byte(line))
		if err != nil {
			out = append(out, events.BatchResult{Error: fmt.Sprintf("decode line: %v", err)})
			continue
		}
		out = append(out, res)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("batch: openai results read: %w", err)
	}
	return out, nil
}

// decodeOpenAIResultLine parses one JSONL line from OpenAI's output file.
// Each line carries either a successful response wrapped in
// {"response":{"status_code":200,"body":{...chat-completion...}}} or an error
// wrapped in {"error":{"code":"...","message":"..."}}.
func decodeOpenAIResultLine(line []byte) (events.BatchResult, error) {
	var raw struct {
		ID       string `json:"id"`
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		} `json:"response,omitempty"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return events.BatchResult{}, err
	}

	br := events.BatchResult{CustomID: raw.CustomID}
	switch {
	case raw.Error != nil && (raw.Error.Code != "" || raw.Error.Message != ""):
		br.Error = fmt.Sprintf("%s: %s", raw.Error.Code, raw.Error.Message)
	case raw.Response != nil:
		if raw.Response.StatusCode < 200 || raw.Response.StatusCode >= 300 {
			br.Error = fmt.Sprintf("http %d: %s", raw.Response.StatusCode, string(raw.Response.Body))
			return br, nil
		}
		resp, err := decodeOpenAIChatBody(raw.Response.Body)
		if err != nil {
			br.Error = fmt.Sprintf("decode body: %v", err)
			return br, nil
		}
		br.Response = resp
	default:
		br.Error = "missing response and error fields"
	}
	return br, nil
}

// decodeOpenAIChatBody maps an OpenAI Chat Completions response onto our
// LLMResponse. Mirrors plugins/providers/openai/plugin.go's convertAPIResponse.
func decodeOpenAIChatBody(raw json.RawMessage) (*events.LLMResponse, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	out := &events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Model: resp.Model,
		Usage: events.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CachedTokens:     resp.Usage.PromptTokensDetails.CachedTokens,
			ReasoningTokens:  resp.Usage.CompletionTokensDetails.ReasoningTokens,
		},
	}
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		out.FinishReason = choice.FinishReason
		if choice.Message.Content != nil {
			out.Content = *choice.Message.Content
		}
		for _, tc := range choice.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, events.ToolCallRequest{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	return out, nil
}

// buildOpenAIChatBody is the minimal LLMRequest -> Chat Completions adapter.
// Mirrors buildAnthropicMessageBody — text-only messages, basic tools/tool-
// choice, response_format passthrough. Multimodal, predicted outputs,
// reasoning effort, and Azure auth are OUT OF SCOPE for v1.
func buildOpenAIChatBody(req events.LLMRequest, defaultMaxTokens int) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("model is required for openai batch requests")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	body := map[string]any{
		"model": req.Model,
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	apiMessages := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "user":
			apiMessages = append(apiMessages, map[string]any{"role": msg.Role, "content": msg.Content})
		case "assistant":
			m := map[string]any{"role": "assistant"}
			if msg.Content != "" {
				m["content"] = msg.Content
			}
			if len(msg.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					calls = append(calls, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": tc.Arguments,
						},
					})
				}
				m["tool_calls"] = calls
			}
			apiMessages = append(apiMessages, m)
		case "tool":
			apiMessages = append(apiMessages, map[string]any{
				"role":         "tool",
				"tool_call_id": msg.ToolCallID,
				"content":      msg.Content,
			})
		default:
			apiMessages = append(apiMessages, map[string]any{"role": msg.Role, "content": msg.Content})
		}
	}
	body["messages"] = apiMessages

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
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

	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			body["response_format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			body["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   rf.Name,
					"schema": rf.Schema,
					"strict": rf.Strict,
				},
			}
		}
	}

	return body, nil
}

// openaiFilesURL composes the absolute URL for a /v1/files endpoint suffix.
// Tests override openaiFilesBaseURL.
func (p *Plugin) openaiFilesURL(suffix string) string {
	base := p.openaiFilesBaseURL
	if base == "" {
		base = openaiFilesURL
	}
	return base + suffix
}

// openaiBatchesURL composes the absolute URL for a /v1/batches endpoint
// suffix. Tests override openaiBatchesBaseURL.
func (p *Plugin) openaiBatchesURL(suffix string) string {
	base := p.openaiBatchesBaseURL
	if base == "" {
		base = openaiBatchesURL
	}
	return base + suffix
}

// applyOpenAIHeaders attaches the bearer token. /v1/files multipart requests
// rely on the caller setting Content-Type explicitly to the multipart boundary,
// so this helper deliberately leaves Content-Type alone.
func (p *Plugin) applyOpenAIHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.openaiAPIKey)
}
