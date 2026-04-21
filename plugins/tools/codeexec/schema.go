package codeexec

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

// schemaToType converts a JSON Schema fragment into a reflect.Type.
//
// Supported keywords:
//   - type: "string" | "integer" | "number" | "boolean" | "array" | "object"
//   - properties: field declarations (object only)
//   - required: required-field list (object only; used for pointer vs value encoding)
//   - items: element schema (array only)
//   - enum: validated at runtime, not at type level (always string/int base)
//
// Unsupported (documented limitation):
//   - oneOf / anyOf / allOf → any (interface{})
//   - additionalProperties → ignored; extra fields unreachable from typed Go
//
// name is used to construct deterministic field names for nested anonymous
// structs and debugging output. Pass the root tool name for top-level calls.
func schemaToType(schema map[string]any, name string) (reflect.Type, error) {
	if schema == nil {
		return reflect.TypeOf((*any)(nil)).Elem(), nil
	}

	// Multi-variant schemas defeat static typing — fall back to any.
	if _, ok := schema["oneOf"]; ok {
		return reflect.TypeOf((*any)(nil)).Elem(), nil
	}
	if _, ok := schema["anyOf"]; ok {
		return reflect.TypeOf((*any)(nil)).Elem(), nil
	}
	if _, ok := schema["allOf"]; ok {
		return reflect.TypeOf((*any)(nil)).Elem(), nil
	}

	typ, _ := schema["type"].(string)
	switch typ {
	case "string":
		return reflect.TypeOf(""), nil
	case "integer":
		return reflect.TypeOf(int64(0)), nil
	case "number":
		return reflect.TypeOf(float64(0)), nil
	case "boolean":
		return reflect.TypeOf(false), nil
	case "array":
		itemSchema, _ := schema["items"].(map[string]any)
		elemType, err := schemaToType(itemSchema, name+"Item")
		if err != nil {
			return nil, fmt.Errorf("array items %s: %w", name, err)
		}
		return reflect.SliceOf(elemType), nil
	case "object", "":
		return objectSchemaToStruct(schema, name)
	default:
		return reflect.TypeOf((*any)(nil)).Elem(), nil
	}
}

// objectSchemaToStruct builds a struct type from an "object" schema. Property
// order is sorted alphabetically so the generated struct layout is stable
// across runs — Yaegi caches types by identity and relies on this.
func objectSchemaToStruct(schema map[string]any, name string) (reflect.Type, error) {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		// Schema-less object — fall back to map for flexibility.
		return reflect.TypeOf((map[string]any)(nil)), nil
	}

	requiredSet := map[string]bool{}
	if reqList, ok := schema["required"].([]any); ok {
		for _, r := range reqList {
			if s, ok := r.(string); ok {
				requiredSet[s] = true
			}
		}
	}
	if reqList, ok := schema["required"].([]string); ok {
		for _, r := range reqList {
			requiredSet[r] = true
		}
	}

	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fields := make([]reflect.StructField, 0, len(keys))
	for _, k := range keys {
		sub, _ := props[k].(map[string]any)
		fieldType, err := schemaToType(sub, name+exportedName(k))
		if err != nil {
			return nil, fmt.Errorf("property %s.%s: %w", name, k, err)
		}

		jsonTag := k
		if !requiredSet[k] {
			jsonTag += ",omitempty"
		}

		fields = append(fields, reflect.StructField{
			Name: exportedName(k),
			Type: fieldType,
			Tag:  reflect.StructTag(fmt.Sprintf(`json:%q`, jsonTag)),
		})
	}

	return reflect.StructOf(fields), nil
}

// exportedName converts a JSON field name to an exported Go identifier.
// "command" → "Command", "max_output_bytes" → "MaxOutputBytes", "2fa_code" → "X2faCode".
func exportedName(s string) string {
	if s == "" {
		return "Field"
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' '
	})
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	out := b.String()
	if out == "" || !unicode.IsLetter(rune(out[0])) {
		out = "X" + out
	}
	return out
}

// toolFuncName produces the exported Go function name for a tool.
// "shell" → "Shell", "file_read" → "FileRead".
func toolFuncName(tool string) string {
	return exportedName(tool)
}

// toolArgsTypeName and toolResultTypeName produce the exported Go type names
// for a tool's arguments and result struct. Matches toolFuncName conventions.
func toolArgsTypeName(tool string) string   { return toolFuncName(tool) + "Args" }
func toolResultTypeName(tool string) string { return toolFuncName(tool) + "Result" }
