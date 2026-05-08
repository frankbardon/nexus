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
		"llm.response", "llm.stream.chunk", "llm.stream.end",
		"before:core.error", "core.error",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
