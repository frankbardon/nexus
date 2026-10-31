package thinking

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// markerSubscriptions exempts the inventory-only declarations: the plugin is a
// feature flag whose Subscriptions() are retained for registry/manifest tooling
// and deliberately never registered (the journal is the source of truth).
var markerSubscriptions = contract.WithConditionalSubscriptions("thinking.step", "plan.progress")

func TestContract_DeclaresThinkingSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New, markerSubscriptions)
	h.AssertSubscribesTo("thinking.step", "plan.progress")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty (journal-only observer)", got)
	}
}

func TestContract_NoEmissionsOnInjectedEvents(t *testing.T) {
	h := contract.NewContract(t, New, markerSubscriptions)
	h.Inject("thinking.step", map[string]any{"content": "test"})
	h.Inject("plan.progress", map[string]any{"step": 1})
	h.AssertNoUndeclaredEmissions()
	if got := h.PluginEmissions(); len(got) != 0 {
		t.Errorf("plugin should be silent observer; got %v", got)
	}
}
