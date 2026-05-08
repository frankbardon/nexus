package tool_result_clear

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke", "tool.result", "agent.turn.end", "before:llm.request")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"memory.tool_result_cleared", "memory.curated"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
