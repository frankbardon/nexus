package gemini

import "testing"

// Regression: MCP servers built with zod-to-json-schema (and any JSON Schema
// 2020-12 producer) emit nullable fields as a "type" union, e.g.
// ["string","null"]. Gemini's Schema.type is a singular, non-repeating enum
// and rejects that shape outright with a 400 ("Proto field is not
// repeating, cannot start list."). Values arrive as []any because they pass
// through json.Unmarshal in plugins/mcp/client/tools.go.
func TestSanitizeSchemaForGemini_CollapsesTypeUnion(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": []any{"string", "null"},
			},
			"count": map[string]any{
				"type": []any{"null", "integer"},
			},
		},
	}

	out := sanitizeSchemaForGemini(in)

	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties not a map: %#v", out["properties"])
	}

	name, ok := props["name"].(map[string]any)
	if !ok {
		t.Fatalf("name property not a map: %#v", props["name"])
	}
	if name["type"] != "string" {
		t.Errorf("name.type = %#v, want %q", name["type"], "string")
	}
	if name["nullable"] != true {
		t.Errorf("name.nullable = %#v, want true", name["nullable"])
	}

	count, ok := props["count"].(map[string]any)
	if !ok {
		t.Fatalf("count property not a map: %#v", props["count"])
	}
	if count["type"] != "integer" {
		t.Errorf("count.type = %#v, want %q", count["type"], "integer")
	}
	if count["nullable"] != true {
		t.Errorf("count.nullable = %#v, want true", count["nullable"])
	}
}

func TestSanitizeSchemaForGemini_PlainTypeUnaffected(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
		"required": []string{"command"},
	}

	out := sanitizeSchemaForGemini(in)

	if out["type"] != "object" {
		t.Errorf("type = %#v, want %q", out["type"], "object")
	}
	if _, ok := out["nullable"]; ok {
		t.Errorf("nullable should be absent, got %#v", out["nullable"])
	}
	props := out["properties"].(map[string]any)
	command := props["command"].(map[string]any)
	if command["type"] != "string" {
		t.Errorf("command.type = %#v, want %q", command["type"], "string")
	}
}
