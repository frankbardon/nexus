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

// TestContract_DeclaredSubscriptionMatchesActualWiring exercises the
// previously-broken slash-command interception: a "/resume" io.input is
// dispatched through EmitVetoable on before:io.input so the cancel
// handler's *VetoablePayload type assertion succeeds. Without a
// previously-cancelled turn, the handler emits a system io.output
// ("Nothing to resume.") and vetoes the dispatch.
func TestContract_DeclaredSubscriptionMatchesActualWiring(t *testing.T) {
	h := contract.NewContract(t, New)

	res := h.InjectVetoable("before:io.input", &events.UserInput{Content: "/resume"})
	if !res.Vetoed {
		t.Fatal("expected /resume to be vetoed when no cancelled turn exists")
	}
	h.AssertEmitted("io.output")
}

// TestContract_ResumeAfterCancel asserts that /resume after a real cancel
// emits a cancel.resume event and vetoes the io.input.
func TestContract_ResumeAfterCancel(t *testing.T) {
	h := contract.NewContract(t, New)

	// Build state: turn started, cancel requested.
	h.Inject("agent.turn.start", events.TurnInfo{TurnID: "turn-1"})
	h.Inject("cancel.request", events.CancelRequest{TurnID: "turn-1"})

	res := h.InjectVetoable("before:io.input", &events.UserInput{Content: "/resume"})
	if !res.Vetoed {
		t.Fatal("expected /resume to be vetoed after a cancel")
	}
	h.AssertEmitted("cancel.resume")
}
