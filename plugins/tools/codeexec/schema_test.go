package codeexec

import (
	"reflect"
	"testing"
)

func TestSchemaToType_Primitives(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   reflect.Kind
	}{
		{"string", map[string]any{"type": "string"}, reflect.String},
		{"integer", map[string]any{"type": "integer"}, reflect.Int64},
		{"number", map[string]any{"type": "number"}, reflect.Float64},
		{"boolean", map[string]any{"type": "boolean"}, reflect.Bool},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typ, err := schemaToType(c.schema, "T")
			if err != nil {
				t.Fatal(err)
			}
			if typ.Kind() != c.want {
				t.Fatalf("got %v, want %v", typ.Kind(), c.want)
			}
		})
	}
}

func TestSchemaToType_Object(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{"type": "integer"},
		},
		"required": []any{"command"},
	}
	typ, err := schemaToType(schema, "Shell")
	if err != nil {
		t.Fatal(err)
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("want struct, got %v", typ.Kind())
	}
	if typ.NumField() != 2 {
		t.Fatalf("want 2 fields, got %d", typ.NumField())
	}

	// Fields sorted alphabetically: command first, timeout second.
	f0 := typ.Field(0)
	if f0.Name != "Command" || f0.Type.Kind() != reflect.String {
		t.Fatalf("field 0: %+v", f0)
	}
	if f0.Tag.Get("json") != "command" {
		t.Fatalf("want command, got %q", f0.Tag.Get("json"))
	}

	f1 := typ.Field(1)
	if f1.Name != "Timeout" || f1.Type.Kind() != reflect.Int64 {
		t.Fatalf("field 1: %+v", f1)
	}
	if f1.Tag.Get("json") != "timeout,omitempty" {
		t.Fatalf("want timeout,omitempty, got %q", f1.Tag.Get("json"))
	}
}

func TestSchemaToType_Array(t *testing.T) {
	schema := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
	typ, err := schemaToType(schema, "Paths")
	if err != nil {
		t.Fatal(err)
	}
	if typ.Kind() != reflect.Slice {
		t.Fatalf("want slice, got %v", typ.Kind())
	}
	if typ.Elem().Kind() != reflect.String {
		t.Fatalf("want []string, got %v", typ)
	}
}

func TestSchemaToType_NestedObject(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filter": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include": map[string]any{"type": "string"},
				},
				"required": []any{"include"},
			},
		},
		"required": []any{"filter"},
	}
	typ, err := schemaToType(schema, "Outer")
	if err != nil {
		t.Fatal(err)
	}
	inner := typ.Field(0).Type
	if inner.Kind() != reflect.Struct || inner.NumField() != 1 {
		t.Fatalf("want nested struct with 1 field, got %v", inner)
	}
	if inner.Field(0).Name != "Include" {
		t.Fatalf("want Include, got %s", inner.Field(0).Name)
	}
}

func TestSchemaToType_OneOfFallsBackToAny(t *testing.T) {
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		},
	}
	typ, err := schemaToType(schema, "U")
	if err != nil {
		t.Fatal(err)
	}
	if typ.Kind() != reflect.Interface {
		t.Fatalf("want interface{}, got %v", typ.Kind())
	}
}

func TestSchemaToType_EmptyObjectIsMap(t *testing.T) {
	typ, err := schemaToType(map[string]any{"type": "object"}, "X")
	if err != nil {
		t.Fatal(err)
	}
	if typ.Kind() != reflect.Map {
		t.Fatalf("want map, got %v", typ.Kind())
	}
}

func TestExportedName(t *testing.T) {
	cases := map[string]string{
		"command":          "Command",
		"file_read":        "FileRead",
		"max-output-bytes": "MaxOutputBytes",
		"2fa_code":         "X2faCode",
		"":                 "Field",
	}
	for in, want := range cases {
		if got := exportedName(in); got != want {
			t.Errorf("exportedName(%q) = %q, want %q", in, got, want)
		}
	}
}
