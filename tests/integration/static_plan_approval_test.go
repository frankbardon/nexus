//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestStaticPlanApproval_Boot validates the static planner + ReAct agent
// boot with planning enabled.
func TestStaticPlanApproval_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-static-plan-approval.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.planner.static",
		"nexus.agent.react",
	)
}

// TestStaticPlanApproval_PlanCreatedAndApproved validates the approve path:
// plan.created fires with the configured 3 steps, plan.approval.request blocks
// until io.test approves, then plan.result emits with Approved=true.
func TestStaticPlanApproval_PlanCreatedAndApproved(t *testing.T) {
	h := testharness.New(t, "configs/test-static-plan-approval.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertEventEmitted("plan.created")
	h.AssertEventEmitted("plan.approval.request")
	h.AssertEventEmitted("plan.result")

	// Verify plan.created carries all 3 configured steps in order.
	var created *events.PlanResult
	for _, e := range h.Events() {
		if e.Type == "plan.created" {
			pr, ok := e.Payload.(events.PlanResult)
			if !ok {
				t.Fatalf("plan.created payload is not events.PlanResult, got %T", e.Payload)
			}
			created = &pr
			break
		}
	}
	if created == nil {
		t.Fatal("plan.created event payload not found")
	}
	if created.Source != "static" {
		t.Errorf("expected plan source=static, got %q", created.Source)
	}
	if len(created.Steps) != 3 {
		t.Fatalf("expected 3 plan steps, got %d", len(created.Steps))
	}
	wantDescs := []string{
		"Read and understand the codebase",
		"Identify issues",
		"Propose fixes",
	}
	for i, want := range wantDescs {
		if created.Steps[i].Description != want {
			t.Errorf("step %d: expected description %q, got %q", i+1, want, created.Steps[i].Description)
		}
	}

	// Verify final plan.result reports approval.
	var result *events.PlanResult
	for _, e := range h.Events() {
		if e.Type == "plan.result" {
			pr, ok := e.Payload.(events.PlanResult)
			if !ok {
				continue
			}
			result = &pr
		}
	}
	if result == nil {
		t.Fatal("plan.result event payload not found")
	}
	if !result.Approved {
		t.Errorf("expected plan.result.Approved=true, got false")
	}
}

// TestStaticPlanApproval_RejectStopsExecution validates the deny path: when
// io.test rejects the approval prompt, plan.result emits with Approved=false
// and no agent tool invocations occur.
func TestStaticPlanApproval_RejectStopsExecution(t *testing.T) {
	cfg := copyConfig(t, "configs/test-static-plan-approval.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"Please review the codebase."},
			"input_delay":   "200ms",
			"approval_mode": "deny",
			"timeout":       "20s",
			"mock_responses": []map[string]any{
				{"content": "(should not run after rejection)"},
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertEventEmitted("plan.created")
	h.AssertEventEmitted("plan.approval.request")
	h.AssertEventEmitted("plan.result")

	var result *events.PlanResult
	for _, e := range h.Events() {
		if e.Type == "plan.result" {
			pr, ok := e.Payload.(events.PlanResult)
			if !ok {
				continue
			}
			result = &pr
		}
	}
	if result == nil {
		t.Fatal("plan.result event payload not found")
	}
	if result.Approved {
		t.Errorf("expected plan.result.Approved=false after rejection, got true")
	}

	// No tools should fire on the rejection path.
	h.AssertToolNotCalled("shell")
	h.AssertToolNotCalled("file_read")
	h.AssertToolNotCalled("list_files")
}
