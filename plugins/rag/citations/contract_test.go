package citations

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("rag.retrieved", "llm.response", "agent.turn.end")
	if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "llm.response.cited" {
		t.Errorf("Emissions() = %v, want [llm.response.cited]", got)
	}
}
