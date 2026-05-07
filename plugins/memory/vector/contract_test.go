package vector

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
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
