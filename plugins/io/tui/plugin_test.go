package tui

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// io/tui owns a terminal so behavioral tests are out of scope for the
// contract harness. Smoke-test the static contract only.
func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New)
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}
