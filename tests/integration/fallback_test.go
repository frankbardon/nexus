//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestFallback_Boot validates that the engine boots cleanly with a fallback
// chain config and the fallback coordinator plugin active.
func TestFallback_Boot(t *testing.T) {
	requireAnthropic(t)
	h := testharness.New(t, "configs/test-fallback.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.llm.openai",
		"nexus.provider.fallback",
		"nexus.agent.react",
		"nexus.memory.capped",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}

// TestFallback_TriggersOnPrimaryFailure validates that when the primary
// provider fails (OpenAI with bogus base_url), the fallback plugin
// intercepts the error and re-routes to the secondary provider (Anthropic).
func TestFallback_TriggersOnPrimaryFailure(t *testing.T) {
	requireAnthropic(t)
	h := testharness.New(t, "configs/test-fallback.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	// Fallback plugin should have emitted these events.
	h.AssertEventEmitted("io.output.clear")
	h.AssertEventEmitted("provider.fallback")

	// Agent should still produce output (via fallback provider).
	h.AssertEventEmitted("io.output")

	// The LLM response should have come through (from Anthropic fallback).
	h.AssertEventEmitted("llm.response")
}
