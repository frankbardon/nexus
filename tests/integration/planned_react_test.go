//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestPlannedReact_Boot validates the ReAct + dynamic planner stack boots.
func TestPlannedReact_Boot(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-planned-react-tools.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.agent.react",
		"nexus.planner.dynamic",
		"nexus.observe.thinking",
	)
}

// TestPlannedReact_PlanThenExecute validates the full plan → execute handoff:
// agent emits plan.request, planner emits plan.created + plan.result, agent
// then runs to a final turn output.
func TestPlannedReact_PlanThenExecute(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-planned-react-tools.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertEventEmitted("plan.request")
	h.AssertEventEmitted("plan.created")
	h.AssertEventEmitted("plan.result")
	h.AssertEventEmitted("agent.turn.end")

	// Verify plan.result was approved (config sets approval=never → auto-approve).
	var approved bool
	for _, e := range h.Events() {
		if e.Type == "plan.result" {
			if pr, ok := e.Payload.(events.PlanResult); ok && pr.Approved {
				approved = true
				break
			}
		}
	}
	if !approved {
		t.Error("expected an approved plan.result, but none seen")
	}

	// Final assistant output should exist.
	var assistantOutput bool
	for _, e := range h.Events() {
		if e.Type != "io.output" {
			continue
		}
		if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "assistant" && len(out.Content) > 0 {
			assistantOutput = true
			break
		}
	}
	if !assistantOutput {
		t.Error("expected at least one non-empty assistant io.output")
	}
}

// TestPlannedReact_ThinkingStepsEmitted validates the planner emits
// thinking.step events that the thinking observer would persist. Avoids
// asserting the on-disk JSONL artifact because the harness cleans the session
// directory on success.
func TestPlannedReact_ThinkingStepsEmitted(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-planned-react-tools.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertEventEmitted("thinking.step")

	// At least one step should be sourced from the planner.
	var fromPlanner int
	for _, e := range h.Events() {
		if e.Type != "thinking.step" {
			continue
		}
		if step, ok := e.Payload.(events.ThinkingStep); ok {
			if step.Source == "nexus.planner.dynamic" {
				fromPlanner++
			}
		}
	}
	if fromPlanner == 0 {
		t.Error("expected at least one thinking.step from nexus.planner.dynamic")
	}
}
