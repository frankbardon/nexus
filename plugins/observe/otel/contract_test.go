package otel

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	// otel uses SubscribeAll rather than declared subscriptions; both
	// Subscriptions() and Emissions() return nil by design.
	if got := h.Plugin().Subscriptions(); len(got) != 0 {
		t.Errorf("Subscriptions() = %v, want nil (uses SubscribeAll)", got)
	}
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want nil (silent observer)", got)
	}
}
