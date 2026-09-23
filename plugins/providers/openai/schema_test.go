package openai

import (
	"encoding/json"
	"testing"
)

// schemaReasoningEnum pulls one enum out of the embedded schema.json's
// `reasoning` block.
func schemaReasoningEnum(t *testing.T, key string) []string {
	t.Helper()

	var doc struct {
		Properties struct {
			Reasoning struct {
				AdditionalProperties *bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Type string   `json:"type"`
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"reasoning"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(configSchemaBytes, &doc); err != nil {
		t.Fatalf("schema.json is not valid JSON: %v", err)
	}
	if doc.Properties.Reasoning.AdditionalProperties == nil || *doc.Properties.Reasoning.AdditionalProperties {
		t.Fatal(`reasoning block must keep "additionalProperties": false`)
	}
	prop, ok := doc.Properties.Reasoning.Properties[key]
	if !ok {
		t.Fatalf("schema.json declares no reasoning.%s", key)
	}
	return prop.Enum
}

func assertSameSet(t *testing.T, what string, code, schema []string) {
	t.Helper()

	inSchema := make(map[string]bool, len(schema))
	for _, v := range schema {
		inSchema[v] = true
	}
	inCode := make(map[string]bool, len(code))
	for _, v := range code {
		inCode[v] = true
	}
	for _, v := range code {
		if !inSchema[v] {
			t.Errorf("%s: the parser accepts %q but schema.json rejects it", what, v)
		}
	}
	for _, v := range schema {
		if !inCode[v] {
			t.Errorf("%s: schema.json accepts %q but the parser rejects it", what, v)
		}
	}
}

// The whole point of E3-S1: code, schema.json and the reference have to agree.
// This pins the code/schema half — a key the parser reads that schema.json does
// not declare is rejected by config validation before the parser ever sees it,
// which is exactly how `reasoning.effort` came to be unreachable.
func TestSchemaMatchesReasoningParser(t *testing.T) {
	assertSameSet(t, "reasoning.effort", reasoningEfforts, schemaReasoningEnum(t, "effort"))
	assertSameSet(t, "reasoning.summary", reasoningSummaries, schemaReasoningEnum(t, "summary"))
	assertSameSet(t, "reasoning.mode",
		[]string{string(reasoningModeEffort), string(reasoningModeOff)},
		schemaReasoningEnum(t, "mode"))

	// The deprecated aliases must stay declared, or a config carrying them
	// fails validation instead of getting the warning the parser raises.
	for _, key := range []string{"enabled", "budget_tokens"} {
		var doc struct {
			Properties struct {
				Reasoning struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"reasoning"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(configSchemaBytes, &doc); err != nil {
			t.Fatalf("schema.json is not valid JSON: %v", err)
		}
		if _, ok := doc.Properties.Reasoning.Properties[key]; !ok {
			t.Errorf("schema.json must keep the deprecated reasoning.%s key declared", key)
		}
	}
}
