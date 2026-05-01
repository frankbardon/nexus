//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestMixedProviders_Boot validates both Anthropic and OpenAI provider plugins
// boot side by side without conflict.
func TestMixedProviders_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-mixed-providers.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.llm.anthropic",
		"nexus.llm.openai",
		"nexus.agent.react",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}
