package tooltimeout

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke", "tool.result", "before:tool.result")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"tool.result", "tool.timeout"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
