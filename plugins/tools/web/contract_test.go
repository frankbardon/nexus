package web

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.invoke", "io.session.end")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"before:tool.result", "tool.result", "tool.register", "search.request"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
