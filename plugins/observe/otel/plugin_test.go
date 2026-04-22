package otel

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"

	"go.opentelemetry.io/otel/attribute"
)

func TestParseConfig_Defaults(t *testing.T) {
	cfg := parseConfig(nil)

	if cfg.endpoint != "localhost:4317" {
		t.Errorf("endpoint = %q, want localhost:4317", cfg.endpoint)
	}
	if cfg.protocol != "grpc" {
		t.Errorf("protocol = %q, want grpc", cfg.protocol)
	}
	if !cfg.insecure {
		t.Error("insecure = false, want true")
	}
	if cfg.serviceName != "nexus" {
		t.Errorf("service_name = %q, want nexus", cfg.serviceName)
	}
	if len(cfg.excludeEvents) != 0 {
		t.Errorf("exclude_events = %v, want empty", cfg.excludeEvents)
	}
}

func TestParseConfig_Override(t *testing.T) {
	raw := map[string]any{
		"endpoint":     "collector:4318",
		"protocol":     "http",
		"insecure":     false,
		"service_name": "myapp",
		"exclude_events": []any{
			"core.tick",
			"llm.stream.chunk",
		},
	}
	cfg := parseConfig(raw)

	if cfg.endpoint != "collector:4318" {
		t.Errorf("endpoint = %q, want collector:4318", cfg.endpoint)
	}
	if cfg.protocol != "http" {
		t.Errorf("protocol = %q, want http", cfg.protocol)
	}
	if cfg.insecure {
		t.Error("insecure = true, want false")
	}
	if cfg.serviceName != "myapp" {
		t.Errorf("service_name = %q, want myapp", cfg.serviceName)
	}
	if len(cfg.excludeEvents) != 2 {
		t.Fatalf("exclude_events len = %d, want 2", len(cfg.excludeEvents))
	}
}

func TestParseConfig_InvalidProtocol(t *testing.T) {
	raw := map[string]any{"protocol": "websocket"}
	cfg := parseConfig(raw)
	if cfg.protocol != "grpc" {
		t.Errorf("invalid protocol should fall back to grpc, got %q", cfg.protocol)
	}
}

func TestIsExcluded_ExactMatch(t *testing.T) {
	p := &Plugin{cfg: config{excludeEvents: []string{"core.tick", "llm.stream.chunk"}}}

	if !p.isExcluded("core.tick") {
		t.Error("core.tick should be excluded")
	}
	if !p.isExcluded("llm.stream.chunk") {
		t.Error("llm.stream.chunk should be excluded")
	}
	if p.isExcluded("llm.request") {
		t.Error("llm.request should not be excluded")
	}
}

func TestIsExcluded_WildcardPrefix(t *testing.T) {
	p := &Plugin{cfg: config{excludeEvents: []string{"llm.stream.*"}}}

	if !p.isExcluded("llm.stream.chunk") {
		t.Error("llm.stream.chunk should match llm.stream.*")
	}
	if !p.isExcluded("llm.stream.end") {
		t.Error("llm.stream.end should match llm.stream.*")
	}
	if p.isExcluded("llm.request") {
		t.Error("llm.request should not match llm.stream.*")
	}
}

func TestIsExcluded_Empty(t *testing.T) {
	p := &Plugin{cfg: config{}}
	if p.isExcluded("anything") {
		t.Error("empty exclude list should exclude nothing")
	}
}

func TestExtractAttributes_LLMRequest(t *testing.T) {
	temp := 0.7
	req := &events.LLMRequest{
		Role:        "balanced",
		Model:       "claude-sonnet-4-20250514",
		MaxTokens:   8192,
		Stream:      true,
		Messages:    []events.Message{{Role: "user", Content: "hi"}},
		Tools:       []events.ToolDef{{Name: "shell"}},
		Temperature: &temp,
	}

	attrs := extractAttributes(req)
	assertAttr(t, attrs, "nexus.llm.role", "balanced")
	assertAttr(t, attrs, "nexus.llm.model", "claude-sonnet-4-20250514")
	assertAttrInt(t, attrs, "nexus.llm.max_tokens", 8192)
	assertAttrBool(t, attrs, "nexus.llm.stream", true)
	assertAttrInt(t, attrs, "nexus.llm.message_count", 1)
	assertAttrInt(t, attrs, "nexus.llm.tool_count", 1)
	assertAttrFloat(t, attrs, "nexus.llm.temperature", 0.7)
}

func TestExtractAttributes_LLMResponse(t *testing.T) {
	resp := &events.LLMResponse{
		Model:        "claude-sonnet-4-20250514",
		FinishReason: "end_turn",
		Usage:        events.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		ToolCalls:    []events.ToolCallRequest{{ID: "tc1", Name: "shell"}},
	}

	attrs := extractAttributes(resp)
	assertAttr(t, attrs, "nexus.llm.model", "claude-sonnet-4-20250514")
	assertAttrInt(t, attrs, "nexus.llm.usage.total_tokens", 150)
	assertAttrInt(t, attrs, "nexus.llm.tool_call_count", 1)
}

func TestExtractAttributes_ToolResult_WithError(t *testing.T) {
	result := &events.ToolResult{
		ID:     "tc1",
		Name:   "shell",
		Error:  "command failed",
		TurnID: "turn-1",
	}

	attrs := extractAttributes(result)
	assertAttrBool(t, attrs, "nexus.tool.has_error", true)
	assertAttr(t, attrs, "nexus.tool.error", "command failed")
}

func TestExtractAttributes_ToolResult_NoError(t *testing.T) {
	result := &events.ToolResult{
		ID:     "tc1",
		Name:   "shell",
		Output: "ok",
		TurnID: "turn-1",
	}

	attrs := extractAttributes(result)
	assertAttrBool(t, attrs, "nexus.tool.has_error", false)
	// Should not have error attr.
	for _, a := range attrs {
		if string(a.Key) == "nexus.tool.error" {
			t.Error("should not have nexus.tool.error when no error")
		}
	}
}

func TestExtractAttributes_VetoablePayload(t *testing.T) {
	req := &events.LLMRequest{Role: "balanced", Model: "test"}
	vp := &engine.VetoablePayload{
		Original: req,
		Veto:     engine.VetoResult{Vetoed: true, Reason: "rate limited"},
	}

	attrs := extractAttributes(vp)
	assertAttrBool(t, attrs, "nexus.veto.vetoed", true)
	assertAttr(t, attrs, "nexus.veto.reason", "rate limited")
	// Should also have LLM attrs from unwrapped payload.
	assertAttr(t, attrs, "nexus.llm.role", "balanced")
}

func TestExtractAttributes_UnknownPayload(t *testing.T) {
	attrs := extractAttributes("some string")
	if len(attrs) != 0 {
		t.Errorf("unknown payload should return nil attrs, got %d", len(attrs))
	}
}

// helpers

func findAttr(attrs []attribute.KeyValue, key string) (attribute.KeyValue, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a, true
		}
	}
	return attribute.KeyValue{}, false
}

func assertAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	a, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if got := a.Value.AsString(); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func assertAttrInt(t *testing.T, attrs []attribute.KeyValue, key string, want int) {
	t.Helper()
	a, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if got := a.Value.AsInt64(); got != int64(want) {
		t.Errorf("%s = %d, want %d", key, got, want)
	}
}

func assertAttrBool(t *testing.T, attrs []attribute.KeyValue, key string, want bool) {
	t.Helper()
	a, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if got := a.Value.AsBool(); got != want {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}

func assertAttrFloat(t *testing.T, attrs []attribute.KeyValue, key string, want float64) {
	t.Helper()
	a, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if got := a.Value.AsFloat64(); got != want {
		t.Errorf("%s = %f, want %f", key, got, want)
	}
}
