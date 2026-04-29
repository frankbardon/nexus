package anthropic

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// TestParseStructuredOutputsConfig_Defaults verifies that an absent
// `structured_outputs` block yields the safe "tool" simulation default with no
// beta header.
func TestParseStructuredOutputsConfig_Defaults(t *testing.T) {
	cfg := map[string]any{}

	out := parseStructuredOutputsConfig(cfg)

	if out.Mode != "tool" {
		t.Errorf("Mode: got %q, want %q", out.Mode, "tool")
	}
	if out.BetaHeader != "" {
		t.Errorf("BetaHeader: got %q, want empty", out.BetaHeader)
	}
}

// TestParseStructuredOutputsConfig_ExplicitTool verifies an explicit
// `mode: tool` setting parses cleanly (no fallback noise).
func TestParseStructuredOutputsConfig_ExplicitTool(t *testing.T) {
	cfg := map[string]any{
		"structured_outputs": map[string]any{
			"mode": "tool",
		},
	}

	out := parseStructuredOutputsConfig(cfg)

	if out.Mode != "tool" {
		t.Errorf("Mode: got %q, want %q", out.Mode, "tool")
	}
}

// TestParseStructuredOutputsConfig_ExplicitNative verifies the native opt-in.
func TestParseStructuredOutputsConfig_ExplicitNative(t *testing.T) {
	cfg := map[string]any{
		"structured_outputs": map[string]any{
			"mode": "native",
		},
	}

	out := parseStructuredOutputsConfig(cfg)

	if out.Mode != "native" {
		t.Errorf("Mode: got %q, want %q", out.Mode, "native")
	}
}

// TestParseStructuredOutputsConfig_InvalidModeFallsBack verifies that an
// unrecognized mode value falls back to "tool" rather than silently enabling
// native (operators have to spell it correctly to opt in).
func TestParseStructuredOutputsConfig_InvalidModeFallsBack(t *testing.T) {
	cfg := map[string]any{
		"structured_outputs": map[string]any{
			"mode": "bogus",
		},
	}

	out := parseStructuredOutputsConfig(cfg)

	if out.Mode != "tool" {
		t.Errorf("Mode: got %q, want fallback %q", out.Mode, "tool")
	}
}

// TestParseStructuredOutputsConfig_BetaHeader verifies the optional beta
// header parses and round-trips.
func TestParseStructuredOutputsConfig_BetaHeader(t *testing.T) {
	cfg := map[string]any{
		"structured_outputs": map[string]any{
			"mode":        "native",
			"beta_header": "output-format-2025-12-01",
		},
	}

	out := parseStructuredOutputsConfig(cfg)

	if out.Mode != "native" {
		t.Errorf("Mode: got %q, want %q", out.Mode, "native")
	}
	if out.BetaHeader != "output-format-2025-12-01" {
		t.Errorf("BetaHeader: got %q, want %q", out.BetaHeader, "output-format-2025-12-01")
	}
}

// TestBuildRequestBody_StructuredOutput_ToolMode verifies the legacy
// simulation behavior is preserved when mode == "tool" (default): a synthetic
// `_structured_output` tool is appended and tool_choice is forced to it.
func TestBuildRequestBody_StructuredOutput_ToolMode(t *testing.T) {
	p := &Plugin{
		logger:            silentLogger(),
		structuredOutputs: structuredOutputsConfig{Mode: "tool"},
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
	}

	req := events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Schema: schema,
		},
	}

	body := p.buildRequestBody("claude-sonnet-4-5-20250514", 1024, req)

	if _, ok := body["response_format"]; ok {
		t.Errorf("tool mode should NOT set response_format on body, got %+v", body["response_format"])
	}

	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 synthetic tool, got %+v", body["tools"])
	}
	if tools[0]["name"] != "_structured_output" {
		t.Errorf("tool name: got %v, want %q", tools[0]["name"], "_structured_output")
	}

	tc, ok := body["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice map, got %+v", body["tool_choice"])
	}
	if tc["type"] != "tool" || tc["name"] != "_structured_output" {
		t.Errorf("tool_choice: got %+v, want forced _structured_output", tc)
	}
}

// TestBuildRequestBody_StructuredOutput_NativeMode verifies the native path:
// response_format set, no synthetic tool, no forced tool_choice.
func TestBuildRequestBody_StructuredOutput_NativeMode(t *testing.T) {
	p := &Plugin{
		logger:            silentLogger(),
		structuredOutputs: structuredOutputsConfig{Mode: "native"},
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
	}

	req := events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Schema: schema,
		},
	}

	body := p.buildRequestBody("claude-sonnet-4-5-20250514", 1024, req)

	rf, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_format map, got %+v", body["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type: got %v, want %q", rf["type"], "json_schema")
	}
	if _, ok := rf["schema"].(map[string]any); !ok {
		t.Errorf("response_format.schema: got %T, want map[string]any", rf["schema"])
	}

	if tools, ok := body["tools"].([]map[string]any); ok {
		for _, tool := range tools {
			if tool["name"] == "_structured_output" {
				t.Errorf("native mode should NOT inject synthetic tool, found %+v", tool)
			}
		}
	}

	if tc, ok := body["tool_choice"]; ok {
		t.Errorf("native mode should not force tool_choice when caller didn't ask, got %+v", tc)
	}
}

// TestBuildRequestBody_StructuredOutput_NativeMode_PreservesToolChoice
// verifies the native path leaves a caller-supplied tool_choice intact rather
// than overriding it with the synthetic tool selection.
func TestBuildRequestBody_StructuredOutput_NativeMode_PreservesToolChoice(t *testing.T) {
	p := &Plugin{
		logger:            silentLogger(),
		structuredOutputs: structuredOutputsConfig{Mode: "native"},
	}

	schema := map[string]any{"type": "object"}

	req := events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
		Tools:    []events.ToolDef{{Name: "shell", Description: "run a command"}},
		ToolChoice: &events.ToolChoice{
			Mode: "tool",
			Name: "shell",
		},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Schema: schema,
		},
	}

	body := p.buildRequestBody("claude-sonnet-4-5-20250514", 1024, req)

	tc, ok := body["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice map, got %+v", body["tool_choice"])
	}
	if tc["type"] != "tool" || tc["name"] != "shell" {
		t.Errorf("tool_choice: got %+v, want caller's {type:tool, name:shell}", tc)
	}

	if rf, ok := body["response_format"].(map[string]any); !ok || rf["type"] != "json_schema" {
		t.Errorf("response_format should still be set in native mode, got %+v", body["response_format"])
	}
}

// TestBetaFlags_NativeStructuredOutputsHeader verifies the operator-supplied
// beta header is appended to anthropic-beta only when native mode is active.
func TestBetaFlags_NativeStructuredOutputsHeader(t *testing.T) {
	p := &Plugin{
		logger: silentLogger(),
		structuredOutputs: structuredOutputsConfig{
			Mode:       "native",
			BetaHeader: "output-format-2025-12-01",
		},
	}

	got := p.betaFlags(nil)

	if !strings.Contains(got, "output-format-2025-12-01") {
		t.Errorf("betaFlags: got %q, want it to contain %q", got, "output-format-2025-12-01")
	}
}

// TestBetaFlags_ToolModeOmitsHeader verifies the beta header is suppressed in
// tool mode even when the operator left a stale value in config.
func TestBetaFlags_ToolModeOmitsHeader(t *testing.T) {
	p := &Plugin{
		logger: silentLogger(),
		structuredOutputs: structuredOutputsConfig{
			Mode:       "tool",
			BetaHeader: "output-format-2025-12-01",
		},
	}

	got := p.betaFlags(nil)

	if strings.Contains(got, "output-format-2025-12-01") {
		t.Errorf("betaFlags: got %q, expected no native header in tool mode", got)
	}
}
