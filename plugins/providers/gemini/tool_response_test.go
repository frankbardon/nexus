package gemini

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// findFunctionResponse returns the response payload of the first
// functionResponse part in the converted contents, or nil.
func findFunctionResponse(t *testing.T, contents []map[string]any) map[string]any {
	t.Helper()
	for _, c := range contents {
		parts, _ := c["parts"].([]map[string]any)
		for _, p := range parts {
			if fr, ok := p["functionResponse"].(map[string]any); ok {
				resp, ok := fr["response"].(map[string]any)
				if !ok {
					t.Fatalf("functionResponse.response is %T, want object (map)", fr["response"])
				}
				return resp
			}
		}
	}
	t.Fatal("no functionResponse part found")
	return nil
}

func convertTool(t *testing.T, toolContent string) map[string]any {
	t.Helper()
	msgs := []events.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []events.ToolCallRequest{{ID: "call1", Name: "pulse_label_resolve", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "call1", Content: toolContent},
	}
	_, contents, err := (&Plugin{}).convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	return findFunctionResponse(t, contents)
}

// A tool result that is a top-level JSON array (e.g. pulse_label_resolve
// returning a list of matches) must be wrapped in an object — Gemini rejects a
// bare array at functionResponse.response.
func TestConvertMessages_WrapsArrayToolResult(t *testing.T) {
	resp := convertTool(t, `[{"name":"Nike","score":1.0},{"name":"Adidas","score":0.8}]`)
	arr, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("array result not wrapped under output: %#v", resp)
	}
	if len(arr) != 2 {
		t.Fatalf("want 2 matches preserved, got %d", len(arr))
	}
}

// A scalar JSON result is likewise wrapped.
func TestConvertMessages_WrapsScalarToolResult(t *testing.T) {
	resp := convertTool(t, `42`)
	if resp["output"] != float64(42) {
		t.Fatalf("scalar result not wrapped: %#v", resp)
	}
}

// An object result passes through unchanged (no double-wrapping).
func TestConvertMessages_PassesObjectToolResult(t *testing.T) {
	resp := convertTool(t, `{"matches":["Nike"],"count":1}`)
	if _, wrapped := resp["output"]; wrapped {
		t.Fatalf("object result should not be wrapped under output: %#v", resp)
	}
	if resp["count"] != float64(1) {
		t.Fatalf("object fields not preserved: %#v", resp)
	}
}

// Non-JSON text is wrapped under output (existing behaviour, guarded).
func TestConvertMessages_WrapsPlainTextToolResult(t *testing.T) {
	resp := convertTool(t, `not json at all`)
	if resp["output"] != "not json at all" {
		t.Fatalf("plain text not wrapped: %#v", resp)
	}
}
