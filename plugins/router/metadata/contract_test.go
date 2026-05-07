package metadata

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("before:llm.request")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (mutates LLMRequest in place)", got)
	}
}
