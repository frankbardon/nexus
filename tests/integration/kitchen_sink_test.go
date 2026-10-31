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
	// The harness redirects the sessions and storage roots, not plugin
	// paths, so point longterm memory at a temp dir to keep it out of ~/.nexus.
	cfg := copyConfig(t, "configs/test-kitchen-sink.yaml", map[string]any{
		"nexus.memory.longterm": map[string]any{
			"scope":     "global",
			"auto_load": false,
			"path":      t.TempDir(),
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
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
		"nexus.observe.thinking",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}
