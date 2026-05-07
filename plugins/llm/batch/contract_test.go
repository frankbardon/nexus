package batch

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo("llm.batch.submit")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"llm.batch.status", "llm.batch.results"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
