package anthropic

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestBetaFlags_StandingOnly checks that the helper joins standing plugin
// flags (cache 1h, files API, pdf_beta) without any per-request additions.
// The order is deterministic: cache → files → pdf_beta.
func TestBetaFlags_StandingOnly(t *testing.T) {
	p := &Plugin{
		logger: silentLogger(),
		cache:  cacheConfig{Enabled: true, TTL: "1h"},
		files:  filesConfig{Enabled: true},
		multimodal: multimodalConfig{
			PDFBeta: true,
		},
	}

	got := p.betaFlags(nil)
	want := "extended-cache-ttl-2025-04-11," + filesAPIBetaHeader + ",pdfs-2024-09-25"
	if got != want {
		t.Errorf("betaFlags(nil) = %q, want %q", got, want)
	}
}

// TestBetaFlags_MetadataOnly checks the per-request additions path with no
// standing flags active.
func TestBetaFlags_MetadataOnly(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	meta := map[string]any{
		"_anthropic_beta_headers": []string{
			"computer-use-2025-01-24",
			"code-execution-2025-05-22",
		},
	}
	got := p.betaFlags(meta)
	want := "computer-use-2025-01-24,code-execution-2025-05-22"
	if got != want {
		t.Errorf("betaFlags(meta) = %q, want %q", got, want)
	}
}

// TestBetaFlags_BothMerged covers the production case: the plugin has
// standing flags and a server-tool plugin contributes per-request ones.
// Standing flags lead, additions follow.
func TestBetaFlags_BothMerged(t *testing.T) {
	p := &Plugin{
		logger: silentLogger(),
		files:  filesConfig{Enabled: true},
	}
	meta := map[string]any{
		"_anthropic_beta_headers": []string{"code-execution-2025-05-22"},
	}

	got := p.betaFlags(meta)
	want := filesAPIBetaHeader + ",code-execution-2025-05-22"
	if got != want {
		t.Errorf("betaFlags = %q, want %q", got, want)
	}
}

// TestBetaFlags_Deduplicates checks that a flag appearing as both a standing
// flag and a per-request flag (or twice in the per-request list) only emits
// once. Duplicates would inflate the header value silently.
func TestBetaFlags_Deduplicates(t *testing.T) {
	p := &Plugin{
		logger: silentLogger(),
		files:  filesConfig{Enabled: true},
	}
	meta := map[string]any{
		"_anthropic_beta_headers": []string{
			filesAPIBetaHeader, // duplicate of the standing files flag
			"code-execution-2025-05-22",
			"code-execution-2025-05-22", // duplicate of the previous
		},
	}

	got := p.betaFlags(meta)
	want := filesAPIBetaHeader + ",code-execution-2025-05-22"
	if got != want {
		t.Errorf("betaFlags = %q, want %q", got, want)
	}
}

// TestBetaFlags_Empty: no standing, no metadata → empty string so callers
// can omit the header entirely.
func TestBetaFlags_Empty(t *testing.T) {
	p := &Plugin{logger: silentLogger()}
	if got := p.betaFlags(nil); got != "" {
		t.Errorf("betaFlags(nil) = %q, want \"\"", got)
	}
	if got := p.betaFlags(map[string]any{}); got != "" {
		t.Errorf("betaFlags(empty) = %q, want \"\"", got)
	}
}

// TestBetaFlags_AnyTyped covers the post-JSON-unmarshal path where the
// metadata array decodes as []any of strings rather than []string.
func TestBetaFlags_AnyTyped(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	meta := map[string]any{
		"_anthropic_beta_headers": []any{
			"bash-2025-01-24",
			"text-editor-2025-07-28",
			42, // non-string element should be silently skipped
		},
	}
	got := p.betaFlags(meta)
	want := "bash-2025-01-24,text-editor-2025-07-28"
	if got != want {
		t.Errorf("betaFlags = %q, want %q", got, want)
	}
}

// TestAppendExtraTools_Typed verifies the in-process []map[string]any path
// — what server-tool plugins write directly when they construct request
// metadata in Go code.
func TestAppendExtraTools_Typed(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	body := map[string]any{
		"tools": []map[string]any{
			{"name": "client_tool", "description": "x", "input_schema": map[string]any{}},
		},
	}
	meta := map[string]any{
		"_anthropic_extra_tools": []map[string]any{
			{"type": "bash_20250124", "name": "bash"},
			{"type": "text_editor_20250728", "name": "str_replace_based_edit_tool"},
		},
	}

	p.appendExtraTools(body, meta)

	tools, ok := body["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("body[\"tools\"] = %T, want []map[string]any", body["tools"])
	}
	if len(tools) != 3 {
		t.Fatalf("tools len = %d, want 3", len(tools))
	}
	if tools[1]["type"] != "bash_20250124" {
		t.Errorf("tools[1][type] = %v, want bash_20250124", tools[1]["type"])
	}
	if tools[2]["name"] != "str_replace_based_edit_tool" {
		t.Errorf("tools[2][name] = %v, want str_replace_based_edit_tool", tools[2]["name"])
	}
}

// TestAppendExtraTools_AnyTyped covers the post-JSON case: metadata round-
// tripped through json.Marshal/Unmarshal yields []any of map[string]any
// elements rather than the typed slice.
func TestAppendExtraTools_AnyTyped(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	body := map[string]any{}
	meta := map[string]any{
		"_anthropic_extra_tools": []any{
			map[string]any{"type": "code_execution_20250522", "name": "code_execution"},
		},
	}

	p.appendExtraTools(body, meta)

	tools, ok := body["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("body[\"tools\"] = %T, want []map[string]any", body["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	if tools[0]["type"] != "code_execution_20250522" {
		t.Errorf("tools[0][type] = %v, want code_execution_20250522", tools[0]["type"])
	}
}

// TestAppendExtraTools_MixedTypes confirms non-map elements in the []any
// case are skipped and a warn log is recorded so operators can debug.
func TestAppendExtraTools_MixedTypes(t *testing.T) {
	logBuf := new(bytes.Buffer)
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	body := map[string]any{}
	meta := map[string]any{
		"_anthropic_extra_tools": []any{
			"this-is-not-a-map",
			map[string]any{"type": "bash_20250124", "name": "bash"},
			42,
		},
	}

	p.appendExtraTools(body, meta)

	tools, ok := body["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("body[\"tools\"] = %T, want []map[string]any", body["tools"])
	}
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1 (only the map should be appended)", len(tools))
	}
	if !strings.Contains(logBuf.String(), "skipping non-map entry") {
		t.Errorf("expected warn log for non-map entries; got: %s", logBuf.String())
	}
}

// TestAppendExtraTools_Absent verifies the no-op path: nothing in metadata,
// so body[\"tools\"] stays untouched.
func TestAppendExtraTools_Absent(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	body := map[string]any{
		"tools": []map[string]any{
			{"name": "client_tool", "description": "x", "input_schema": map[string]any{}},
		},
	}
	p.appendExtraTools(body, nil)
	p.appendExtraTools(body, map[string]any{})

	tools := body["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Errorf("tools len = %d, want 1 (untouched)", len(tools))
	}
}

// TestConvertAPIResponse_CodeExecutionToolResult verifies that a sync
// response containing a code_execution_tool_result block lands on the
// response Metadata as a server_tool_results entry, with the inner content
// preserved verbatim (json.RawMessage) so per-server-tool plugins can decode
// it without us having to model every wire shape.
func TestConvertAPIResponse_CodeExecutionToolResult(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	innerContent := `{"type":"code_execution_result","stdout":"hello\n","stderr":"","return_code":0}`
	apiResp := apiResponse{
		ID:    "msg_test",
		Model: "claude-sonnet-4-5-20250514",
		Content: []apiContentBlock{
			{Type: "text", Text: "Running:"},
			{
				Type:      "code_execution_tool_result",
				ToolUseID: "srvtoolu_abc123",
				Content:   json.RawMessage(innerContent),
			},
		},
		StopReason: "end_turn",
	}

	resp := p.convertAPIResponse(apiResp)

	if resp.Content != "Running:" {
		t.Errorf("Content = %q, want %q", resp.Content, "Running:")
	}

	if resp.Metadata == nil {
		t.Fatal("Metadata is nil; expected server_tool_results entry")
	}
	results, ok := resp.Metadata["server_tool_results"].([]map[string]any)
	if !ok {
		t.Fatalf("server_tool_results type = %T, want []map[string]any", resp.Metadata["server_tool_results"])
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0]["type"] != "code_execution_tool_result" {
		t.Errorf("type = %v, want code_execution_tool_result", results[0]["type"])
	}
	if results[0]["tool_use_id"] != "srvtoolu_abc123" {
		t.Errorf("tool_use_id = %v, want srvtoolu_abc123", results[0]["tool_use_id"])
	}
	gotContent, ok := results[0]["content"].(json.RawMessage)
	if !ok {
		t.Fatalf("content type = %T, want json.RawMessage", results[0]["content"])
	}
	if string(gotContent) != innerContent {
		t.Errorf("content = %q, want %q", string(gotContent), innerContent)
	}
}

// TestConvertAPIResponse_NoServerToolResults makes sure the non-server-tool
// path doesn't accidentally introduce a server_tool_results key when there
// were no such blocks. Belt-and-suspenders against future regressions.
func TestConvertAPIResponse_NoServerToolResults(t *testing.T) {
	p := &Plugin{logger: silentLogger()}

	apiResp := apiResponse{
		ID:    "msg_plain",
		Model: "claude-sonnet-4-5-20250514",
		Content: []apiContentBlock{
			{Type: "text", Text: "hello"},
		},
		StopReason: "end_turn",
	}

	resp := p.convertAPIResponse(apiResp)
	if resp.Metadata != nil {
		if _, present := resp.Metadata["server_tool_results"]; present {
			t.Errorf("server_tool_results should be absent when no server-tool blocks are present")
		}
	}
}
