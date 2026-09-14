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

// Regression: Gemini rejects any array-typed schema without "items"
// ("GenerateContentRequest.tools[0].function_declarations[13].parameters
// .properties[patch].items: missing field."). JSON Schema leaves items
// optional, so MCP servers emit free-form arrays (RFC 6902 patch ops, opaque
// value lists) with no element schema at all.
func TestSanitizeSchemaForGemini_FillsMissingArrayItems(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"patch": map[string]any{
				"type":        "array",
				"description": "RFC 6902 ops",
			},
			"matrix": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "array"},
			},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}

	out := sanitizeSchemaForGemini(in)
	props := out["properties"].(map[string]any)

	patchItems, ok := props["patch"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("patch.items not a map: %#v", props["patch"])
	}
	if len(patchItems) != 0 {
		t.Errorf("patch.items = %#v, want empty schema", patchItems)
	}

	// Nested arrays need items at every level too.
	outer := props["matrix"].(map[string]any)["items"].(map[string]any)
	inner, ok := outer["items"].(map[string]any)
	if !ok {
		t.Fatalf("matrix.items.items not a map: %#v", outer)
	}
	if len(inner) != 0 {
		t.Errorf("matrix.items.items = %#v, want empty schema", inner)
	}

	// A declared element schema is left alone.
	tags := props["tags"].(map[string]any)["items"].(map[string]any)
	if tags["type"] != "string" {
		t.Errorf("tags.items.type = %#v, want %q", tags["type"], "string")
	}
}
