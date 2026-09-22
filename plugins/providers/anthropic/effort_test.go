package anthropic

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// --- parseOutputConfig ------------------------------------------------------

// TestParseOutputConfig_Absent verifies that omitting the `output_config` block
// leaves the effort unset, so nothing is emitted and the API default applies.
func TestParseOutputConfig_Absent(t *testing.T) {
	oc, err := parseOutputConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parseOutputConfig: unexpected error: %v", err)
	}
	if oc.Effort != effortUnset {
		t.Fatalf("effort = %q, want unset", oc.Effort)
	}
}

// TestParseOutputConfig_BlockWithoutEffort verifies that a present block with
// no `effort` key is the same as no block at all.
func TestParseOutputConfig_BlockWithoutEffort(t *testing.T) {
	oc, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{},
	})
	if err != nil {
		t.Fatalf("parseOutputConfig: unexpected error: %v", err)
	}
	if oc.Effort != effortUnset {
		t.Fatalf("effort = %q, want unset", oc.Effort)
	}
}

// TestParseOutputConfig_AcceptedLevels pins the closed vocabulary: exactly the
// five levels Anthropic documents, no more.
func TestParseOutputConfig_AcceptedLevels(t *testing.T) {
	for _, want := range []effortLevel{effortLow, effortMedium, effortHigh, effortXHigh, effortMax} {
		oc, err := parseOutputConfig(map[string]any{
			"output_config": map[string]any{"effort": string(want)},
		})
		if err != nil {
			t.Fatalf("effort %q: unexpected error: %v", want, err)
		}
		if oc.Effort != want {
			t.Fatalf("effort = %q, want %q", oc.Effort, want)
		}
	}
}

// TestParseOutputConfig_UnknownLevel verifies an invalid value fails at parse
// (hence Init) time with an error naming the accepted values, rather than
// reaching the wire as an Anthropic 400.
func TestParseOutputConfig_UnknownLevel(t *testing.T) {
	_, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{"effort": "extreme"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown effort level")
	}
	for _, want := range []string{"extreme", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestParseOutputConfig_NonStringLevel verifies a wrongly-typed value is a
// config error rather than a silent drop.
func TestParseOutputConfig_NonStringLevel(t *testing.T) {
	_, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{"effort": 3},
	})
	if err == nil {
		t.Fatal("expected an error for a non-string effort level")
	}
	if !strings.Contains(err.Error(), "output_config.effort") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// --- applyEffort ------------------------------------------------------------

// TestApplyEffort_Unset verifies that an unset effort adds no `output_config`
// key at all, leaving the API default (`high`) in force.
func TestApplyEffort_Unset(t *testing.T) {
	body := map[string]any{"model": "claude-opus-4-5"}
	applyEffort(body, effortUnset)
	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config present for an unset effort: %#v", body["output_config"])
	}
}

// TestApplyEffort_Nesting pins the wire shape: effort is nested inside
// `output_config`, never a top-level request field.
func TestApplyEffort_Nesting(t *testing.T) {
	body := map[string]any{}
	applyEffort(body, effortXHigh)

	if _, ok := body["effort"]; ok {
		t.Fatal("effort written as a top-level body field; it must nest in output_config")
	}
	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "xhigh" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "xhigh")
	}
}

// TestApplyEffort_MergesIntoExisting verifies the emission merges into an
// `output_config` object that is already on the body rather than replacing it.
// Nothing else writes that object today — the native structured-output path
// still emits a stale top-level `response_format` — but when that is corrected
// `format` and `effort` must coexist.
func TestApplyEffort_MergesIntoExisting(t *testing.T) {
	body := map[string]any{
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_schema"},
		},
	}
	applyEffort(body, effortMax)

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "max" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "max")
	}
	if _, ok := oc["format"]; !ok {
		t.Fatalf("applyEffort clobbered a pre-existing output_config: %#v", oc)
	}
}

// --- buildRequestBody -------------------------------------------------------

// TestBuildRequestBody_EffortOnWire verifies the provider-level key reaches the
// request body through the normal build path.
func TestBuildRequestBody_EffortOnWire(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "low" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "low")
	}
}

// TestBuildRequestBody_NoEffortNoOutputConfig verifies the unset default adds
// nothing, so an existing deployment's request body is byte-identical.
func TestBuildRequestBody_NoEffortNoOutputConfig(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config present with no effort configured: %#v", body["output_config"])
	}
}

// TestBuildRequestBody_EffortIndependentOfThinkingMode verifies effort is
// emitted under every thinking mode, including `off`. The two are separate
// controls: one sets how hard the model works, the other whether thinking
// blocks come back.
func TestBuildRequestBody_EffortIndependentOfThinkingMode(t *testing.T) {
	modes := []thinkingMode{thinkingModeOff, thinkingModeAdaptive, thinkingModeDisabled, thinkingModeBudget}

	for _, mode := range modes {
		p := &Plugin{logger: silentTestLogger()}
		p.outputConfig = outputConfig{Effort: effortHigh}
		p.thinking = thinkingConfig{Mode: mode, BudgetTokens: 2048}

		body := p.buildRequestBody("claude-opus-4-5", 4096, events.LLMRequest{
			Messages: []events.Message{{Role: "user", Content: "hi"}},
		})

		oc, ok := body["output_config"].(map[string]any)
		if !ok {
			t.Fatalf("mode %q: output_config = %#v, want map[string]any", mode, body["output_config"])
		}
		if oc["effort"] != "high" {
			t.Fatalf("mode %q: output_config.effort = %#v, want %q", mode, oc["effort"], "high")
		}
	}
}

// TestBuildRequestBody_EffortSurvivesNativeStructuredOutput is the regression
// guard for the deferred `response_format` → `output_config.format` fix: the
// two emissions must not interfere in either direction.
func TestBuildRequestBody_EffortSurvivesNativeStructuredOutput(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortMedium}
	p.structuredOutputs = structuredOutputsConfig{Mode: "native"}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Schema: map[string]any{"type": "object"},
		},
	})

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "medium" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "medium")
	}
	if _, ok := body["response_format"]; !ok {
		t.Fatal("native structured output was lost")
	}
}
