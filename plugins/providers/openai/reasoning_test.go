package openai

import (
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// silentLogger returns a slog.Logger that discards output, so tests don't
// pollute stdout with debug lines from applyReasoning.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseReasoningConfig_Defaults(t *testing.T) {
	rc := parseReasoningConfig(map[string]any{})
	if rc.Effort != "" {
		t.Errorf("default Effort: got %q, want empty", rc.Effort)
	}
	if rc.IncludeSummary {
		t.Errorf("default IncludeSummary: got true, want false")
	}
}

func TestParseReasoningConfig_ExplicitEfforts(t *testing.T) {
	for _, want := range []string{"minimal", "low", "medium", "high"} {
		cfg := map[string]any{
			"reasoning": map[string]any{
				"effort":          want,
				"include_summary": true,
			},
		}
		rc := parseReasoningConfig(cfg)
		if rc.Effort != want {
			t.Errorf("effort=%q: got %q", want, rc.Effort)
		}
		if !rc.IncludeSummary {
			t.Errorf("effort=%q: IncludeSummary should be true", want)
		}
	}
}

func TestParseReasoningConfig_InvalidEffortIgnored(t *testing.T) {
	cfg := map[string]any{
		"reasoning": map[string]any{
			"effort": "extreme", // not a valid value
		},
	}
	rc := parseReasoningConfig(cfg)
	if rc.Effort != "" {
		t.Errorf("invalid effort: got %q, want empty", rc.Effort)
	}
}

func TestIsReasoningModel(t *testing.T) {
	matches := []string{
		"o1", "o1-mini", "o1-preview",
		"o3", "o3-mini",
		"o4-mini",
		"gpt-5", "gpt-5-mini", "gpt-5-thinking", "gpt-5-mini-thinking",
	}
	for _, m := range matches {
		if !isReasoningModel(m) {
			t.Errorf("isReasoningModel(%q): got false, want true", m)
		}
	}

	misses := []string{
		"gpt-4o", "gpt-4-turbo", "gpt-4o-mini",
		"claude-3-5-sonnet-20241022",
		"",
	}
	for _, m := range misses {
		if isReasoningModel(m) {
			t.Errorf("isReasoningModel(%q): got true, want false", m)
		}
	}
}

func TestApplyReasoning_NonReasoningModelLeavesBodyAlone(t *testing.T) {
	body := map[string]any{
		"model":       "gpt-4o",
		"temperature": 0.7,
		"top_p":       0.9,
	}
	cfg := reasoningConfig{Effort: "medium"}

	applyReasoning(body, "gpt-4o", cfg, false, silentLogger())

	if body["temperature"] != 0.7 {
		t.Errorf("temperature should be untouched: got %v", body["temperature"])
	}
	if body["top_p"] != 0.9 {
		t.Errorf("top_p should be untouched: got %v", body["top_p"])
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort should not be set on non-reasoning model")
	}
}

func TestApplyReasoning_ReasoningModelStripsAndSetsEffort(t *testing.T) {
	body := map[string]any{
		"model":             "o1-mini",
		"temperature":       0.7,
		"top_p":             0.9,
		"presence_penalty":  0.1,
		"frequency_penalty": 0.2,
		"logprobs":          true,
		"top_logprobs":      5,
		"prediction":        map[string]any{"type": "content", "content": "hi"},
		"messages":          []any{},
	}
	cfg := reasoningConfig{Effort: "high"}

	applyReasoning(body, "o1-mini", cfg, false, silentLogger())

	for _, f := range []string{
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"logprobs", "top_logprobs", "prediction",
	} {
		if _, ok := body[f]; ok {
			t.Errorf("field %q should be stripped", f)
		}
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort: got %v, want high", got)
	}
	// Unrelated keys preserved.
	if _, ok := body["messages"]; !ok {
		t.Error("messages should be preserved")
	}
}

func TestApplyReasoning_ForceReasoningOnNonReasoningModel(t *testing.T) {
	body := map[string]any{
		"model":       "gpt-4o",
		"temperature": 0.5,
	}
	cfg := reasoningConfig{Effort: "low"}

	applyReasoning(body, "gpt-4o", cfg, true, silentLogger())

	if _, ok := body["temperature"]; ok {
		t.Error("temperature should be stripped when force_reasoning=true")
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Errorf("reasoning_effort: got %v, want low", got)
	}
}

func TestApplyReasoning_ReasoningModelNoEffortConfig(t *testing.T) {
	body := map[string]any{
		"model":       "o3-mini",
		"temperature": 0.7,
	}
	cfg := reasoningConfig{} // no effort

	applyReasoning(body, "o3-mini", cfg, false, silentLogger())

	if _, ok := body["temperature"]; ok {
		t.Error("temperature should be stripped on reasoning model")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort should not be set when Effort is empty")
	}
}

func TestConvertAPIResponse_PopulatesReasoningTokens(t *testing.T) {
	p := &Plugin{pricing: defaultPricing}

	content := "thinking complete"
	apiResp := apiResponse{
		ID:    "chatcmpl-r-test",
		Model: "o1-mini",
		Choices: []apiChoice{
			{
				Index:        0,
				Message:      apiMessage{Role: "assistant", Content: &content},
				FinishReason: "stop",
			},
		},
	}
	apiResp.Usage.PromptTokens = 100
	apiResp.Usage.CompletionTokens = 800 // includes the 512 reasoning tokens
	apiResp.Usage.TotalTokens = 900
	apiResp.Usage.CompletionTokensDetails.ReasoningTokens = 512

	resp := p.convertAPIResponse(apiResp)

	if resp.Usage.ReasoningTokens != 512 {
		t.Errorf("ReasoningTokens: got %d, want 512", resp.Usage.ReasoningTokens)
	}
	if resp.Usage.CompletionTokens != 800 {
		t.Errorf("CompletionTokens: got %d, want 800", resp.Usage.CompletionTokens)
	}
}

// TestBuildRequestBody_ReasoningModelStripsTemperature exercises the integration
// between buildRequestBody and applyReasoning: an LLMRequest with Temperature
// targeting o1-mini should produce a body with no `temperature` field but with
// the configured `reasoning_effort`.
func TestBuildRequestBody_ReasoningModelStripsTemperature(t *testing.T) {
	temp := 0.7
	p := &Plugin{
		logger:    silentLogger(),
		reasoning: reasoningConfig{Effort: "medium"},
	}

	req := events.LLMRequest{
		Messages: []events.Message{
			{Role: "user", Content: "hello"},
		},
		Temperature: &temp,
	}

	body := p.buildRequestBody("o1-mini", 1024, req)

	if _, ok := body["temperature"]; ok {
		t.Errorf("temperature should be stripped from o1-mini body")
	}
	if got := body["reasoning_effort"]; got != "medium" {
		t.Errorf("reasoning_effort: got %v, want medium", got)
	}
	if got := body["model"]; got != "o1-mini" {
		t.Errorf("model: got %v, want o1-mini", got)
	}
}
