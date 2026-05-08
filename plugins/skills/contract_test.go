package skills

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo(
		"tool.invoke", "skill.activate", "skill.resource.read",
		"skill.deactivate", "before:llm.request",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"skill.discover", "skill.loaded", "skill.resource.result",
		"before:skill.activate", "before:tool.result", "tool.result",
		"tool.register", "schema.register", "schema.deregister",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
