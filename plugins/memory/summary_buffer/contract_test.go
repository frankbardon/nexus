package summary_buffer

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.input", "llm.response", "tool.invoke", "tool.result",
		"agent.turn.end", "memory.history.query", "memory.compact.request",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"llm.request", "memory.compaction.triggered", "memory.compacted",
		"memory.summary_replaced", "memory.curated", "io.status",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
