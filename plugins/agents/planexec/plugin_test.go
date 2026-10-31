package planexec

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
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

// captureLLMRequests records every llm.request published on the harness bus.
func captureLLMRequests(h *contract.ContractHarness) *[]events.LLMRequest {
	var reqs []events.LLMRequest
	h.Bus().Subscribe("llm.request", func(ev engine.Event[any]) {
		if req, ok := ev.Payload.(events.LLMRequest); ok {
			reqs = append(reqs, req)
		}
	})
	return &reqs
}

// TestLLMRequests_StampTurnID pins that both planexec llm.request kinds (step
// execution and synthesis) name the turn they serve in
// Metadata[engine.MetaTurnID]. An approval rule on either copies it onto
// HITLRequest.TurnID; empty, a turn-scoped transport never resumes (#218).
func TestLLMRequests_StampTurnID(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	p := h.Plugin().(*Plugin)
	reqs := captureLLMRequests(h)

	p.mu.Lock()
	p.currentTurnID = "turn-pe"
	p.originalInput = "do the thing"
	p.plan = []planStep{{ID: "s1", Description: "step one", Status: "active"}}
	p.stepResults = map[string]string{}
	p.mu.Unlock()

	p.sendStepLLMRequest()
	p.sendSynthesisRequest()

	if len(*reqs) != 2 {
		t.Fatalf("llm.request count = %d, want 2", len(*reqs))
	}
	for _, req := range *reqs {
		kind := req.Metadata[engine.MetaTaskKind]
		if got := req.Metadata[engine.MetaTurnID]; got != "turn-pe" {
			t.Errorf("%v: Metadata[%q] = %v, want turn-pe", kind, engine.MetaTurnID, got)
		}
	}
}
