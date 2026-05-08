package static

import (
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// busOnlyContext returns a minimal PluginContext suitable for testing
// Init failure paths where the harness can't be used (because the harness
// fails the test on Init error).
func busOnlyContext(t *testing.T) engine.PluginContext {
	t.Helper()
	return engine.PluginContext{
		Bus:    engine.NewEventBus(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: map[string]any{},
	}
}

func twoStepConfig() map[string]any {
	return map[string]any{
		"summary":  "two-step plan",
		"approval": "never",
		"steps": []any{
			map[string]any{"description": "step one", "instructions": "do A"},
			map[string]any{"description": "step two", "instructions": "do B"},
		},
	}
}

func TestContract_DeclaresPlanRequestSubscription(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(twoStepConfig()))
	h.AssertSubscribesTo("plan.request")
	h.AssertSubscribesTo("plan.approval.response")
}

// TestContract_NoSteps_InitFails: NewContract calls Init internally and
// uses t.Fatalf on failure (which Goexits the goroutine), so a sub-T
// approach can't capture the error. Verify the plugin's Init contract
// directly instead — no harness needed for the negative path.
func TestContract_NoSteps_InitFails(t *testing.T) {
	p := New().(*Plugin)
	err := p.Init(busOnlyContext(t))
	if err == nil {
		t.Error("expected Init to fail when no steps configured")
	}
}

func TestContract_PlanRequest_EmitsResultDirectly_WhenApprovalNever(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(twoStepConfig()))

	h.Inject("plan.request", events.PlanRequest{
		SchemaVersion: events.PlanRequestVersion,
		TurnID:        "turn-x",
		SessionID:     "sess-x",
		Input:         "trigger",
	})

	h.AssertEmitted("io.status")
	h.AssertEmitted("thinking.step")
	h.AssertEmitted("plan.created")
	h.AssertEmitted("plan.result")
	// approval=never, so no approval.request.
	h.AssertNotEmitted("plan.approval.request")
}

func TestContract_PlanRequest_EmitsApprovalWhenAlways(t *testing.T) {
	cfg := twoStepConfig()
	cfg["approval"] = "always"
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(cfg))

	h.Inject("plan.request", events.PlanRequest{
		SchemaVersion: events.PlanRequestVersion,
		TurnID:        "turn-y",
		SessionID:     "sess-y",
	})

	h.AssertEmitted("plan.approval.request")
	// plan.result must NOT fire until approval comes back.
	h.AssertNotEmitted("plan.result")
}

func TestContract_NoUndeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(twoStepConfig()))
	h.Inject("plan.request", events.PlanRequest{
		SchemaVersion: events.PlanRequestVersion,
		TurnID:        "turn-z",
	})
	h.AssertNoUndeclaredEmissions()
}
