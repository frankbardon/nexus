package hitlsynthesizer

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("hitl.requested", "before:hitl.requested", "llm.response")
	if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "llm.request" {
		t.Errorf("Emissions() = %v, want [llm.request]", got)
	}
}
