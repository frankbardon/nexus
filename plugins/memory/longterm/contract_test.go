package longterm

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(map[string]any{
			"scope":     "global",
			"auto_load": false,
		}))
	h.AssertSubscribesTo(
		"memory.longterm.store", "memory.longterm.read",
		"memory.longterm.delete", "memory.longterm.list",
		"tool.invoke", "hitl.responded",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"memory.longterm.loaded", "memory.longterm.stored",
		"memory.longterm.result", "memory.longterm.deleted",
		"memory.longterm.list.result",
		"tool.register", "before:tool.result", "tool.result",
		"before:hitl.requested", "hitl.requested",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
