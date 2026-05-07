package classifier

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"classifier_model": "tiny-judge",
		"candidates":       []any{"haiku", "sonnet", "opus"},
		"fallback":         "sonnet",
	}))
	h.AssertSubscribesTo("before:llm.request", "llm.response")
	if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "llm.request" {
		t.Errorf("Emissions() = %v, want [llm.request]", got)
	}
}
