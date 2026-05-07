package simple

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.input", "llm.response", "tool.invoke", "tool.result",
		"memory.history.query", "memory.compacted",
	)
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (simple memory mutates buffer in place)", got)
	}
}
