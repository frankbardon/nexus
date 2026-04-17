package testharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Judge evaluates whether LLM output satisfies given criteria.
// Implementations may use an LLM, local model, or rule-based system.
type Judge interface {
	Evaluate(ctx context.Context, output string, criteria string) (JudgeResult, error)
}

// JudgeResult is the outcome of a semantic evaluation.
type JudgeResult struct {
	Pass   bool
	Reason string
}

// AnthropicJudge uses Claude Haiku to evaluate output against criteria.
type AnthropicJudge struct {
	apiKey string
	model  string
	client *http.Client
}

// NewAnthropicJudge creates a judge using Claude Haiku.
func NewAnthropicJudge(apiKey string) *AnthropicJudge {
	return &AnthropicJudge{
		apiKey: apiKey,
		model:  "claude-haiku-4-5-20251001",
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Evaluate asks Haiku whether the output satisfies the criteria.
// Returns pass/fail with reasoning.
func (j *AnthropicJudge) Evaluate(ctx context.Context, output string, criteria string) (JudgeResult, error) {
	prompt := fmt.Sprintf(`You are an automated test judge. Evaluate whether the following AI assistant output satisfies the given criteria.

<criteria>
%s
</criteria>

<output>
%s
</output>

Respond with a JSON object: {"pass": true/false, "reason": "brief explanation"}`, criteria, output)

	body := map[string]any{
		"model":      j.model,
		"max_tokens": 256,
		"messages": []any{
			map[string]string{"role": "user", "content": prompt},
			map[string]string{"role": "assistant", "content": "{"},
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return JudgeResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", j.apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := j.client.Do(req)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("judge request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return JudgeResult{}, fmt.Errorf("judge API error %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse Anthropic response to extract text content.
	var apiResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return JudgeResult{}, fmt.Errorf("parse API response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return JudgeResult{}, fmt.Errorf("empty judge response")
	}

	// Reconstruct full JSON — we prefilled "{" in the assistant turn.
	raw := "{" + apiResp.Content[0].Text

	// Parse the judge's JSON verdict.
	var result JudgeResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		// Fallback: try to extract JSON from markdown fences or surrounding text.
		if extracted := extractJSON(raw); extracted != "" {
			if err2 := json.Unmarshal([]byte(extracted), &result); err2 == nil {
				return result, nil
			}
		}
		return JudgeResult{
			Pass:   false,
			Reason: fmt.Sprintf("judge returned non-JSON: %s", raw),
		}, nil
	}

	return result, nil
}

// extractJSON tries to pull a JSON object out of text that may contain
// markdown fences or surrounding prose.
func extractJSON(s string) string {
	// Strip markdown code fences.
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	// Find first { and last }.
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return ""
}
