package oneshot

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"prompt": "hello",
	}))
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}
