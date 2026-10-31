package vector

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		// hitl.responded is subscribed lazily by the approval helper only
		// while a require_approval commit is pending, never during Init.
		contract.WithConditionalSubscriptions("hitl.responded"),
	)
	h.AssertSubscribesTo("io.input", "memory.compacted", "memory.vector.store", "hitl.responded")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"embeddings.request", "vector.query", "hybrid.query", "vector.upsert",
		"rag.retrieved", "before:hitl.requested", "hitl.requested",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
