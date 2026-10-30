package gemini

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// Regression (#147): Gemini's Schema.enum is a repeated string valid only on
// STRING schemas. A numeric member ([0.05, 0.1]) or an empty string anywhere
// in any tool schema 400s the whole request, so one bad MCP tool took down
// the entire catalog on every turn.

func newEnumTestPlugin() (*Plugin, *bytes.Buffer) {
	var buf bytes.Buffer
	p := &Plugin{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	return p, &buf
}

func TestSanitizeToolSchema_DropsNumericEnum(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"threshold": map[string]any{
				"type": "number",
				"enum": []any{0.05, 0.1},
			},
		},
	}
	out := p.sanitizeToolSchema("mcp__x__y", in)
	th := out["properties"].(map[string]any)["threshold"].(map[string]any)
	if _, ok := th["enum"]; ok {
		t.Fatalf("numeric enum should be dropped, got %#v", th["enum"])
	}
	if th["type"] != "number" {
		t.Errorf("type = %#v, want number", th["type"])
	}
	if !strings.Contains(logs.String(), `tool \"mcp__x__y\" parameters.properties.threshold.enum`) {
		t.Errorf("warning should name tool and path, got: %s", logs.String())
	}
}

func TestSanitizeToolSchema_NonStringMembersOnUntypedSchema(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rate": map[string]any{"enum": []any{0.05, "fast"}},
			"nums": map[string]any{"enum": []any{1.0, true}},
		},
	}
	out := p.sanitizeToolSchema("t", in)
	props := out["properties"].(map[string]any)
	if got := props["rate"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"fast"}) {
		t.Errorf("rate.enum = %#v, want [fast]", got)
	}
	if _, ok := props["nums"].(map[string]any)["enum"]; ok {
		t.Errorf("enum with no string members should be dropped")
	}
	if !strings.Contains(logs.String(), "parameters.properties.rate.enum[0] is 0.05 (number)") {
		t.Errorf("missing member-level warning, got: %s", logs.String())
	}
}

func TestSanitizeToolSchema_RemovesEmptyStringMember(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"", "a", "b"}},
			"only": map[string]any{"type": "string", "enum": []any{""}},
		},
	}
	out := p.sanitizeToolSchema("t", in)
	props := out["properties"].(map[string]any)
	if got := props["mode"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("mode.enum = %#v, want [a b]", got)
	}
	if _, ok := props["only"].(map[string]any)["enum"]; ok {
		t.Errorf("enum emptied by repair should be dropped")
	}
	if !strings.Contains(logs.String(), "parameters.properties.mode.enum[0] is an empty string") {
		t.Errorf("missing empty-string warning, got: %s", logs.String())
	}
}

func TestSanitizeToolSchema_NestedItemsAndAnyOf(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rows": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"level": map[string]any{"type": "integer", "enum": []any{1.0, 2.0}},
					},
				},
			},
			"pick": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string", "enum": []any{"x", ""}},
					map[string]any{"type": "number", "enum": []any{3.0}},
				},
			},
		},
	}
	out := p.sanitizeToolSchema("t", in)
	props := out["properties"].(map[string]any)
	level := props["rows"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["level"].(map[string]any)
	if _, ok := level["enum"]; ok {
		t.Errorf("nested integer enum should be dropped")
	}
	anyOf := props["pick"].(map[string]any)["anyOf"].([]any)
	if got := anyOf[0].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"x"}) {
		t.Errorf("anyOf[0].enum = %#v, want [x]", got)
	}
	if _, ok := anyOf[1].(map[string]any)["enum"]; ok {
		t.Errorf("anyOf[1] numeric enum should be dropped")
	}
	for _, want := range []string{
		"parameters.properties.rows.items.properties.level.enum",
		"parameters.properties.pick.anyOf[0].enum[1]",
		"parameters.properties.pick.anyOf[1].enum",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing warning for %s, got: %s", want, logs.String())
		}
	}
}

func TestSanitizeToolSchema_ValidStringEnumUntouched(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color": map[string]any{"type": "string", "enum": []any{"red", "green"}},
			// A property literally named "enum" is a schema, not a member list.
			"enum": map[string]any{"type": "string"},
		},
	}
	out := p.sanitizeToolSchema("t", in)
	props := out["properties"].(map[string]any)
	if got := props["color"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"red", "green"}) {
		t.Errorf("color.enum = %#v, want [red green]", got)
	}
	if got := props["enum"].(map[string]any)["type"]; got != "string" {
		t.Errorf("property named enum mangled: %#v", props["enum"])
	}
	if logs.Len() != 0 {
		t.Errorf("valid schema should not warn, got: %s", logs.String())
	}
}

func TestSanitizeToolSchema_WarnsOncePerToolPath(t *testing.T) {
	p, logs := newEnumTestPlugin()
	in := map[string]any{"type": "number", "enum": []any{1.0}}
	p.sanitizeToolSchema("t", in)
	p.sanitizeToolSchema("t", in)
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("warn count = %d, want 1 (deduped across turns)", n)
	}
}
