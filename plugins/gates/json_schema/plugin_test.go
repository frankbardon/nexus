package jsonschema

import (
	"encoding/json"
	"testing"
)

// testPlugin creates a Plugin with a compiled schema from the given map.
func testPlugin(t *testing.T, schema map[string]any) *Plugin {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("failed to marshal test schema: %v", err)
	}
	compiled, err := compileSchema(data)
	if err != nil {
		t.Fatalf("failed to compile test schema: %v", err)
	}
	return &Plugin{
		compiled:  compiled,
		schemaRaw: string(data),
	}
}

func TestValidate_ValidJSON(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type":     "object",
		"required": []any{"name", "age"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "number"},
		},
	})

	err := p.validate(`{"name": "Alice", "age": 30}`)
	if err != "" {
		t.Fatalf("expected valid, got error: %s", err)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type":     "object",
		"required": []any{"name", "age"},
	})

	err := p.validate(`{"name": "Alice"}`)
	if err == "" {
		t.Fatal("expected error for missing required field 'age'")
	}
}

func TestValidate_WrongType(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"age": map[string]any{"type": "number"},
		},
	})

	err := p.validate(`{"age": "thirty"}`)
	if err == "" {
		t.Fatal("expected error for wrong type")
	}
}

func TestValidate_InvalidJSON(t *testing.T) {
	p := testPlugin(t, map[string]any{"type": "object"})

	err := p.validate(`not json at all`)
	if err == "" {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidate_ArrayItems(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "string",
		},
	})

	err := p.validate(`["a", "b", "c"]`)
	if err != "" {
		t.Fatalf("expected valid array, got: %s", err)
	}

	err = p.validate(`["a", 123, "c"]`)
	if err == "" {
		t.Fatal("expected error for non-string array item")
	}
}

func TestValidate_MarkdownWrapped(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type":     "object",
		"required": []any{"name"},
	})

	content := "```json\n{\"name\": \"Alice\"}\n```"
	err := p.validate(content)
	if err != "" {
		t.Fatalf("should extract JSON from markdown, got: %s", err)
	}
}

func TestValidate_Enum(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type": "string",
		"enum": []any{"red", "green", "blue"},
	})

	err := p.validate(`"red"`)
	if err != "" {
		t.Fatalf("expected valid enum value, got: %s", err)
	}

	err = p.validate(`"yellow"`)
	if err == "" {
		t.Fatal("expected error for non-enum value")
	}
}

func TestValidate_NestedObjects(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type":     "object",
		"required": []any{"address"},
		"properties": map[string]any{
			"address": map[string]any{
				"type":     "object",
				"required": []any{"street", "city"},
				"properties": map[string]any{
					"street": map[string]any{"type": "string"},
					"city":   map[string]any{"type": "string"},
					"zip":    map[string]any{"type": "string", "pattern": "^\\d{5}$"},
				},
			},
		},
	})

	err := p.validate(`{"address": {"street": "123 Main", "city": "Springfield", "zip": "62704"}}`)
	if err != "" {
		t.Fatalf("expected valid nested object, got: %s", err)
	}

	err = p.validate(`{"address": {"street": "123 Main"}}`)
	if err == "" {
		t.Fatal("expected error for missing nested required field")
	}

	err = p.validate(`{"address": {"street": "123 Main", "city": "Springfield", "zip": "bad"}}`)
	if err == "" {
		t.Fatal("expected error for pattern mismatch on zip")
	}
}

func TestValidate_AdditionalProperties(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	})

	err := p.validate(`{"name": "Alice"}`)
	if err != "" {
		t.Fatalf("expected valid, got: %s", err)
	}

	err = p.validate(`{"name": "Alice", "extra": true}`)
	if err == "" {
		t.Fatal("expected error for additional property")
	}
}

func TestValidate_MinMaxLength(t *testing.T) {
	p := testPlugin(t, map[string]any{
		"type":      "string",
		"minLength": 2,
		"maxLength": 5,
	})

	err := p.validate(`"abc"`)
	if err != "" {
		t.Fatalf("expected valid, got: %s", err)
	}

	err = p.validate(`"a"`)
	if err == "" {
		t.Fatal("expected error for string too short")
	}

	err = p.validate(`"toolong"`)
	if err == "" {
		t.Fatal("expected error for string too long")
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`},
		{"json block", "```json\n{\"a\": 1}\n```", `{"a": 1}`},
		{"generic block", "```\n{\"a\": 1}\n```", `{"a": 1}`},
		{"with text", "Here is the result:\n```json\n{\"a\": 1}\n```\nDone.", `{"a": 1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.input)
			if got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}
