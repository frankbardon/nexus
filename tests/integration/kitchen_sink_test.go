//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestKitchenSink_Boot validates that every shipping plugin can boot together
// without dependency conflicts, init errors, or event-bus contention.
func TestKitchenSink_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-kitchen-sink.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.agent.react",
		"nexus.agent.subagent",
		"nexus.gate.endless_loop",
		"nexus.gate.stop_words",
		"nexus.gate.token_budget",
		"nexus.gate.rate_limiter",
		"nexus.gate.prompt_injection",
		"nexus.gate.output_length",
		"nexus.gate.content_safety",
		"nexus.gate.context_window",
		"nexus.gate.tool_filter",
		"nexus.tool.shell",
		"nexus.tool.file",
		"nexus.skills",
		"nexus.memory.capped",
		"nexus.memory.longterm",
		"nexus.observe.logger",
		"nexus.observe.thinking",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}
