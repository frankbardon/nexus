//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestWebSearch_AnthropicNative boots the web tool plus the native Anthropic
// search adapter and asks the agent to run a real query. It validates the
// full live path: tool registration → LLM picks web_search → tool plugin
// emits search.request → adapter calls Anthropic → results flow back through
// tool.result. No mocking; requires ANTHROPIC_API_KEY.
func TestWebSearch_AnthropicNative(t *testing.T) {
	requireAnthropic(t)
	h := testharness.New(t, "configs/test-web-search.yaml",
		testharness.WithTimeout(2*time.Minute))
	h.Run()

	// Bootstrap check — both the consumer and the provider must be up.
	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.agent.react",
		"nexus.memory.capped",
		"nexus.tool.web",
		"nexus.search.anthropic_native",
	)

	// The react agent should actually choose web_search — if it doesn't, the
	// system prompt or the model routing is wrong.
	h.AssertToolCalled("web_search")

	// The tool result should contain the provider tag the web tool writes
	// into its formatted output. If this fails, either the adapter didn't
	// answer or the correlation in dispatch is broken.
	h.AssertOutputSemantic(
		"the assistant's final response quotes at least one URL starting with http, " +
			"indicating it actually used web_search results rather than answering from memory",
	)
}
