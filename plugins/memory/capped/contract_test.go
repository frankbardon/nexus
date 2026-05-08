package capped

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.input", "llm.response", "tool.invoke", "tool.result",
		"memory.store", "memory.query", "memory.history.query", "memory.compacted",
	)
	if got := h.Plugin().Emissions(); len(got) != 1 || got[0] != "memory.result" {
		t.Errorf("Emissions() = %v, want [memory.result]", got)
	}
}
