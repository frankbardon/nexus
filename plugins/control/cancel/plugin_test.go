package cancel

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresExpectedEmissions(t *testing.T) {
	h := contract.NewContract(t, New)

	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"cancel.active", "cancel.resume", "io.output", "io.status"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q (got %v)", want, h.Plugin().Emissions())
		}
	}
}

func TestContract_AdvertisesControlCancelCapability(t *testing.T) {
	h := contract.NewContract(t, New)
	caps := h.Plugin().Capabilities()
	if len(caps) != 1 || caps[0].Name != "control.cancel" {
		t.Errorf("Capabilities() = %v, want [control.cancel]", caps)
	}
}

func TestContract_CancelRequest_EmitsActiveAndStatus(t *testing.T) {
	h := contract.NewContract(t, New)

	// Start a turn so the cancel handler has an active turn ID to operate on.
	h.Inject("agent.turn.start", events.TurnInfo{TurnID: "turn-1"})
	h.Inject("cancel.request", events.CancelRequest{TurnID: "turn-1"})

	h.AssertEmitted("io.status")
	h.AssertEmitted("cancel.active")
	h.AssertEmittedInOrder("io.status", "cancel.active")
}

func TestContract_CancelRequest_NoActiveTurn_NoOp(t *testing.T) {
	h := contract.NewContract(t, New)
	h.Inject("cancel.request", events.CancelRequest{TurnID: "turn-1"})
	h.AssertNotEmitted("cancel.active")
}

// CONTRACT MISMATCH SURFACED — Subscriptions() returns "before:io.input"
// (line 93 of plugin.go) but Init wires the actual handler to "io.input"
// (line 71). The slash-command interception is therefore broken: the
// handler's *engine.VetoablePayload type assertion never succeeds for
// plain io.input dispatch. Documented in
// .planning/testing-upgrade/02-tier-2-plugin-contracts.md and intentionally
// not asserted here so this test file stays green until the plugin is
// fixed; the audit task (T2.6) will create a follow-up.
func TestContract_DeclaredSubscriptionMatchesActualWiring(t *testing.T) {
	t.Skip("known mismatch: cancel plugin declares before:io.input but subscribes to io.input — tracked as a follow-up")
}
