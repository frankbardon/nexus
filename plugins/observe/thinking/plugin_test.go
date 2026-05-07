package thinking

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresThinkingSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("thinking.step", "plan.progress")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (journal-only observer)", got)
	}
}

func TestContract_NoEmissionsOnInjectedEvents(t *testing.T) {
	h := contract.NewContract(t, New)
	h.Inject("thinking.step", map[string]any{"content": "test"})
	h.Inject("plan.progress", map[string]any{"step": 1})
	h.AssertNoUndeclaredEmissions()
	if got := h.PluginEmissions(); len(got) != 0 {
		t.Errorf("plugin should be silent observer; got %v", got)
	}
}
