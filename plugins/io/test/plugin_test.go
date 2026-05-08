package testio

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// io/test is the test IO plugin used by integration tests in
// pkg/testharness/. The contract test here just verifies the plugin
// advertises a stable static contract.
func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New)
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	emits := h.Plugin().Emissions()
	if len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}
