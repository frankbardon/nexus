package orchestrator

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	if subs := h.Plugin().Subscriptions(); len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}

// TestLLMRequests_StampTurnID pins that both orchestrator llm.request kinds
// (decompose and synthesize) name the turn they serve in
// Metadata[engine.MetaTurnID]. An approval rule on either copies it onto
// HITLRequest.TurnID; empty, a turn-scoped transport never resumes (#218).
func TestLLMRequests_StampTurnID(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	p := h.Plugin().(*Plugin)
	var reqs []events.LLMRequest
	h.Bus().Subscribe("llm.request", func(ev engine.Event[any]) {
		if req, ok := ev.Payload.(events.LLMRequest); ok {
			reqs = append(reqs, req)
		}
	})

	p.mu.Lock()
	p.currentTurnID = "turn-orch"
	p.originalInput = "do the thing"
	p.mu.Unlock()

	p.sendDecomposeRequest()
	p.sendSynthesizeRequest()

	if len(reqs) != 2 {
		t.Fatalf("llm.request count = %d, want 2", len(reqs))
	}
	for _, req := range reqs {
		kind := req.Metadata[engine.MetaTaskKind]
		if got := req.Metadata[engine.MetaTurnID]; got != "turn-orch" {
			t.Errorf("%v: Metadata[%q] = %v, want turn-orch", kind, engine.MetaTurnID, got)
		}
	}
}
