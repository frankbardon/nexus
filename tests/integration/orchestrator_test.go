//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestOrchestrator_Boot validates orchestrator + worker subagent boot together.
func TestOrchestrator_Boot(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-orchestrator-full.yaml", testharness.WithTimeout(120*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.agent.orchestrator",
		"nexus.agent.subagent/main",
	)
}

// TestOrchestrator_DecomposesAndSpawns validates the orchestrator decomposes
// the task and emits subagent.spawn events for at least one worker.
//
// Decomposition behaviour is LLM-driven and can vary, so this test asserts on
// the structural minimum: at least one worker spawn and a final turn end.
// Cost-sensitive (one orchestrator + worker LLM call each).
func TestOrchestrator_DecomposesAndSpawns(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-orchestrator-full.yaml", testharness.WithTimeout(120*time.Second))
	h.Run()

	h.AssertEventEmitted("subagent.spawn")
	h.AssertEventEmitted("agent.turn.end")

	// Verify spawn payload carries a non-empty task description.
	var spawnTasks int
	for _, e := range h.Events() {
		if e.Type != "subagent.spawn" {
			continue
		}
		if sp, ok := e.Payload.(events.SubagentSpawn); ok && sp.Task != "" {
			spawnTasks++
		}
	}
	if spawnTasks == 0 {
		t.Error("expected at least one subagent.spawn with a non-empty Task")
	}

	// Final assistant output expected.
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
