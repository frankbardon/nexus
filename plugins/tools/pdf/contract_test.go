package pdf

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"before:tool.result", "tool.result", "tool.register"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
