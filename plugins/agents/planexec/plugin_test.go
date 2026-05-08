package planexec

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// Plan-then-execute agents are deeply stateful, integrate planners + LLM
// providers, and exercise multi-turn flows. Comprehensive coverage lives
// in tests/integration/ (planexec_approval_test.go, planned_react_test.go).
// The contract test below asserts the static event-contract surface so any
// additions or removals to Subscriptions/Emissions are caught at unit time.

func TestContract_DeclaredSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo(
		"io.input",
		"tool.result",
		"llm.response",
		"plan.result",
		"plan.approval.response",
	)
}

func TestContract_DeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"llm.request", "tool.invoke", "io.output",
		"agent.turn.start", "agent.turn.end",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
