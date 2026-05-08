package fanout

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"before:llm.request", "llm.response",
		"before:core.error", "provider.fanout.chosen",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"llm.request", "llm.response",
		"provider.fanout.start", "provider.fanout.response",
		"provider.fanout.complete", "provider.fanout.choose",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
