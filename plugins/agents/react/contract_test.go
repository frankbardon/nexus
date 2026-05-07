package react

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo(
		"io.input", "tool.invoke", "tool.result", "llm.response",
		"skill.loaded", "plan.result", "cancel.active", "cancel.resume",
		"gate.llm.retry", "agent.tool_choice",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"llm.request", "tool.invoke", "io.output",
		"agent.turn.start", "agent.turn.end", "plan.request",
		"skill.activate", "cancel.complete",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
