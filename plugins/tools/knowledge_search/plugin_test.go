package knowledge_search

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New,
		contract.WithSession(),
		contract.WithPluginConfig(map[string]any{
			"namespaces": []any{"default"},
		}))
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("expected at least one subscription")
	}
	emits := h.Plugin().Emissions()
	if len(emits) == 0 {
		t.Error("expected at least one declared emission (tool.register, tool.result, ...)")
	}
}
