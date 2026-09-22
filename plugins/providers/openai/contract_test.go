package openai

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"api_key": "sk-mock-not-used",
	}))
	h.AssertSubscribesTo("llm.request", "cancel.active")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"before:llm.response", "llm.response",
		"llm.stream.chunk", "llm.stream.end",
		"before:core.error", "core.error",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}

// TestContract_ReasoningBlockBoots pins the repair in E3-S1: a `reasoning:`
// block in the shape schema.json declares boots the plugin and lands on the
// request body. Before this, the only key the parser read was one schema.json
// rejected, so no operator YAML could put a reasoning control on a request.
func TestContract_ReasoningBlockBoots(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"api_key": "sk-mock-not-used",
		"reasoning": map[string]any{
			"mode":    "effort",
			"effort":  "xhigh",
			"summary": "auto",
		},
	}))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin is %T, want *Plugin", h.Plugin())
	}
	if p.reasoning.Mode != reasoningModeEffort {
		t.Errorf("Mode: got %q, want effort", p.reasoning.Mode)
	}
	if p.reasoning.Effort != "xhigh" {
		t.Errorf("Effort: got %q, want xhigh", p.reasoning.Effort)
	}
	if p.reasoning.Summary != "auto" {
		t.Errorf("Summary: got %q, want auto", p.reasoning.Summary)
	}

	body := map[string]any{}
	applyReasoning(body, "gpt-5", p.reasoning, false, silentLogger())
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Errorf("reasoning_effort on the wire: got %v, want xhigh", got)
	}
}
