package openai

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"api_key": "sk-mock-not-used",
	}))
	h.AssertSubscribesTo("embeddings.request")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (mutates request payload)", got)
	}
}
