//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestMinimal_Boot validates that the engine boots cleanly with the absolute
// minimum config — no tools, no gates, no observers.
func TestMinimal_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-minimal.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertBooted("nexus.io.test", "nexus.llm.anthropic", "nexus.agent.react", "nexus.memory.capped")
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}

// TestMinimal_Conversation validates basic conversational flow with no tools.
func TestMinimal_Conversation(t *testing.T) {
	h := testharness.New(t, "configs/test-minimal.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	// Agent should produce at least one assistant output per input.
	h.AssertEventEmitted("io.output")
	h.AssertEventCount("io.input", 3, 3)

	// No tools available — should not see tool invocations.
	h.AssertEventNotEmitted("tool.invoke")

	// No gates — should not see system-role gate messages.
	h.AssertNoSystemOutput()
}

// TestMinimal_MultiTurn validates that conversation memory works across turns.
func TestMinimal_MultiTurn(t *testing.T) {
	h := testharness.New(t, "configs/test-minimal.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	// Third input asks "What was the first thing I asked you?" — agent should
	// reference the first message. Semantic check validates this.
	h.AssertOutputSemantic("the final response references or recalls the user's first question about greeting or identity")
}
