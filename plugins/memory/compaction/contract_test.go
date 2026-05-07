package compaction

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.input", "io.output", "tool.invoke", "tool.result",
		"agent.turn.end", "llm.response", "memory.compact.request", "hitl.responded",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"llm.request", "memory.compaction.triggered", "memory.compacted",
		"thinking.step", "io.status", "before:hitl.requested", "hitl.requested",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
