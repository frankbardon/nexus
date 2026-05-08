package topic_pruner

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("io.input", "agent.turn.end")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"memory.topic_shift_detected", "memory.curated", "embeddings.request"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
