//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestGeminiThinking_Boot validates the gemini provider + thinking observer
// boot together.
func TestGeminiThinking_Boot(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-gemini-thinking.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	h.AssertBooted("nexus.llm.gemini", "nexus.observe.thinking")
}

// TestGeminiThinking_EmitsReasoningSteps validates that gemini-2.5 with
// thinking enabled emits thinking.step events sourced from the provider, and
// reports reasoning tokens > 0 in the LLM response usage.
func TestGeminiThinking_EmitsReasoningSteps(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-gemini-thinking.yaml", testharness.WithTimeout(90*time.Second))
	h.Run()

	// At least one thinking.step event sourced from the provider.
	var fromGemini int
	for _, e := range h.Events() {
		if e.Type != "thinking.step" {
			continue
		}
		if step, ok := e.Payload.(events.ThinkingStep); ok && step.Source == "nexus.llm.gemini" {
			fromGemini++
		}
	}
	if fromGemini == 0 {
		t.Error("expected at least one thinking.step from nexus.llm.gemini")
	}

	// Final llm.response should report ReasoningTokens > 0.
	var sawReasoningTokens bool
	for _, e := range h.Events() {
		if e.Type != "llm.response" {
			continue
		}
		if resp, ok := e.Payload.(events.LLMResponse); ok && resp.Usage.ReasoningTokens > 0 {
			sawReasoningTokens = true
			break
		}
	}
	if !sawReasoningTokens {
		t.Error("expected at least one llm.response with usage.ReasoningTokens > 0")
	}
}
