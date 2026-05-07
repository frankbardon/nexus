package sqlitefts

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo("lexical.upsert", "lexical.query", "lexical.delete", "lexical.namespace.drop")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (mutates request payload)", got)
	}
}
