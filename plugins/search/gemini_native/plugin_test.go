package geminative

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"api_key": "test-key-not-real",
	}))
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
}
