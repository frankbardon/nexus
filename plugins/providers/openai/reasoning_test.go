package openai

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// silentLogger returns a slog.Logger that discards output, so tests don't
// pollute stdout with debug lines from applyReasoning.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseReasoningConfig_AbsentBlockIsOff(t *testing.T) {
	rc, err := parseReasoningConfig(map[string]any{}, silentLogger())
	if err != nil {
		t.Fatalf("parseReasoningConfig: %v", err)
	}
	if rc.Mode != reasoningModeOff {
		t.Errorf("default Mode: got %q, want off", rc.Mode)
	}
	if rc.Effort != "" {
		t.Errorf("default Effort: got %q, want empty", rc.Effort)
	}
	if rc.Summary != "" {
		t.Errorf("default Summary: got %q, want empty", rc.Summary)
	}
}

// A present block with no `mode` resolves to the one on-mode, mirroring
// thinking on nexus.llm.anthropic and nexus.llm.gemini.
func TestParseReasoningConfig_PresentBlockDefaultsToEffortMode(t *testing.T) {
	rc, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"effort": "high"},
	}, silentLogger())
	if err != nil {
		t.Fatalf("parseReasoningConfig: %v", err)
	}
	if rc.Mode != reasoningModeEffort {
		t.Errorf("Mode: got %q, want effort", rc.Mode)
	}
	if rc.Effort != "high" {
		t.Errorf("Effort: got %q, want high", rc.Effort)
	}
}

// OpenAI's full vocabulary, unclamped: it is a superset of the union of what
// Anthropic and Gemini accept, so every word from either passes through.
func TestParseReasoningConfig_FullEffortVocabulary(t *testing.T) {
	for _, want := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		rc, err := parseReasoningConfig(map[string]any{
			"reasoning": map[string]any{"mode": "effort", "effort": want},
		}, silentLogger())
		if err != nil {
			t.Fatalf("effort=%q: %v", want, err)
		}
		if rc.Effort != want {
			t.Errorf("effort=%q: got %q", want, rc.Effort)
		}
	}
}

func TestParseReasoningConfig_InvalidEffortFailsInit(t *testing.T) {
	_, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"effort": "extreme"},
	}, silentLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown effort")
	}
	for _, want := range []string{"extreme", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
}

func TestParseReasoningConfig_Summary(t *testing.T) {
	for _, want := range []string{"auto", "concise", "detailed"} {
		rc, err := parseReasoningConfig(map[string]any{
			"reasoning": map[string]any{"summary": want},
		}, silentLogger())
		if err != nil {
			t.Fatalf("summary=%q: %v", want, err)
		}
		if rc.Summary != want {
			t.Errorf("summary=%q: got %q", want, rc.Summary)
		}
	}
	if _, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"summary": "verbose"},
	}, silentLogger()); err == nil {
		t.Error("expected an error for an unknown summary")
	}
}

func TestParseReasoningConfig_InvalidModeFailsInit(t *testing.T) {
	_, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"mode": "adaptive"},
	}, silentLogger())
	if err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}

func TestParseReasoningConfig_ModeOff(t *testing.T) {
	rc, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"mode": "off", "effort": "high"},
	}, silentLogger())
	if err != nil {
		t.Fatalf("parseReasoningConfig: %v", err)
	}
	if rc.Mode != reasoningModeOff {
		t.Errorf("Mode: got %q, want off", rc.Mode)
	}

	body := map[string]any{"model": "o1-mini"}
	applyReasoning(body, "o1-mini", rc, false, silentLogger())
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("mode: off must send no reasoning configuration")
	}
}

// The deprecated aliases are accepted rather than failing the boot.
func TestParseReasoningConfig_DeprecatedAliases(t *testing.T) {
	rc, err := parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"enabled": true},
	}, silentLogger())
	if err != nil {
		t.Fatalf("enabled: true: %v", err)
	}
	if rc.Mode != reasoningModeEffort {
		t.Errorf("enabled: true -> Mode %q, want effort", rc.Mode)
	}

	rc, err = parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"enabled": false, "effort": "high"},
	}, silentLogger())
	if err != nil {
		t.Fatalf("enabled: false: %v", err)
	}
	if rc.Mode != reasoningModeOff {
		t.Errorf("enabled: false -> Mode %q, want off", rc.Mode)
	}

	// An explicit mode wins over the alias.
	rc, err = parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"enabled": false, "mode": "effort", "effort": "low"},
	}, silentLogger())
	if err != nil {
		t.Fatalf("mode beats enabled: %v", err)
	}
	if rc.Mode != reasoningModeEffort {
		t.Errorf("mode should win over enabled: got %q", rc.Mode)
	}

	// budget_tokens is accepted, ignored and does not fail the boot.
	rc, err = parseReasoningConfig(map[string]any{
		"reasoning": map[string]any{"budget_tokens": 10000, "effort": "medium"},
	}, silentLogger())
	if err != nil {
		t.Fatalf("budget_tokens: %v", err)
	}
	if rc.Mode != reasoningModeEffort || rc.Effort != "medium" {
		t.Errorf("budget_tokens should be ignored: got mode %q effort %q", rc.Mode, rc.Effort)
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
	cfg := reasoningConfig{Mode: reasoningModeEffort, Effort: "medium"}

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
	cfg := reasoningConfig{Mode: reasoningModeEffort, Effort: "high"}

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
	cfg := reasoningConfig{Mode: reasoningModeEffort, Effort: "low"}

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
	cfg := reasoningConfig{Mode: reasoningModeEffort} // no effort

	applyReasoning(body, "o3-mini", cfg, false, silentLogger())

	if _, ok := body["temperature"]; ok {
		t.Error("temperature should be stripped on reasoning model")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort should not be set when Effort is empty")
	}
}

func TestConvertAPIResponse_PopulatesReasoningTokens(t *testing.T) {
	p := &Plugin{pricing: pricing.DefaultsFor(pricing.ProviderOpenAI)}

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
		reasoning: reasoningConfig{Mode: reasoningModeEffort, Effort: "medium"},
	}

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
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
