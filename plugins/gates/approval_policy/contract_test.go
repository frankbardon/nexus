package approvalpolicy

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("before:tool.invoke", "before:llm.request", "hitl.responded")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"before:hitl.requested", "hitl.requested"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q (got %v)", want, h.Plugin().Emissions())
		}
	}
}
