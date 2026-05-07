package hybrid

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("hybrid.query")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"embeddings.request", "vector.query", "lexical.query", "reranker.rerank"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
