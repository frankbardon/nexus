package ratelimiter

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("before:llm.request")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"io.output", "gate.llm.retry"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
