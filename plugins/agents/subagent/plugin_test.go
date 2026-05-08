package subagent

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaredSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"name":           "researcher",
		"system_prompt":  "You are a researcher.",
		"max_iterations": 3,
	}))
	h.AssertSubscribesTo("tool.invoke", "tool.register")
}

func TestContract_DeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"name":          "researcher",
		"system_prompt": "research",
	}))
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"tool.register",
		"tool.result",
		"llm.request",
		"subagent.started",
		"subagent.complete",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
