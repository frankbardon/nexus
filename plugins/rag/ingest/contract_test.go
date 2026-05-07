package ingest

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo("rag.ingest", "rag.ingest.delete")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"embeddings.request", "vector.upsert", "vector.delete",
		"lexical.upsert", "lexical.delete",
		"rag.ingest", "rag.ingest.delete", "rag.ingest.result",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
