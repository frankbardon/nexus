package classifier

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"classifier_role": "classifier",
		"candidate_roles": []any{"quick", "balanced", "reasoning"},
		"fallback_role":   "balanced",
	}))
	h.AssertSubscribesTo("before:llm.request", "llm.response")
	// Requests are published through engine.PublishLLMRequest, which offers
	// before:llm.request ahead of llm.request.
	if got := h.Plugin().Emissions(); len(got) != 2 || got[0] != "before:llm.request" || got[1] != "llm.request" {
		t.Errorf("Emissions() = %v, want [before:llm.request llm.request]", got)
	}
}
