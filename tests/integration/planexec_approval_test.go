//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestPlanexecApproval_Boot validates the planexec + dynamic planner stack boots.
func TestPlanexecApproval_Boot(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-planexec-approval.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.agent.planexec",
		"nexus.planner.dynamic",
	)
}

// TestPlanexecApproval_ApprovedRunsToCompletion validates the approve path:
// dynamic planner generates a plan, plan.approval.request fires, io.test
// approves, planexec executes the plan and emits agent.turn.end.
func TestPlanexecApproval_ApprovedRunsToCompletion(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-planexec-approval.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertEventEmitted("plan.request")
	h.AssertEventEmitted("plan.created")
	h.AssertEventEmitted("plan.approval.request")
	h.AssertEventEmitted("plan.result")
	h.AssertEventEmitted("agent.turn.end")

	// Final plan.result should report Approved=true.
	var approved bool
	for _, e := range h.Events() {
		if e.Type == "plan.result" {
			if pr, ok := e.Payload.(events.PlanResult); ok && pr.Approved {
				approved = true
			}
		}
	}
	if !approved {
		t.Error("expected an approved plan.result")
	}
}

// TestPlanexecApproval_RejectedStopsExecution validates the deny path:
// plan.approval.request fires, io.test denies, planexec sees plan.result
// with Approved=false and ends the turn without executing.
func TestPlanexecApproval_RejectedStopsExecution(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	cfg := copyConfig(t, "configs/test-planexec-approval.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"Compute 2 plus 2 and explain the answer in one sentence."},
			"input_delay":   "200ms",
			"approval_mode": "deny",
			"timeout":       "90s",
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertEventEmitted("plan.approval.request")
	h.AssertEventEmitted("plan.result")

	// Final plan.result should report Approved=false.
	var sawRejection bool
	for _, e := range h.Events() {
		if e.Type == "plan.result" {
			if pr, ok := e.Payload.(events.PlanResult); ok && !pr.Approved {
				sawRejection = true
			}
		}
	}
	if !sawRejection {
		t.Error("expected plan.result with Approved=false on rejection path")
	}
}
