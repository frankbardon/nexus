//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestGemini_Boot validates the Gemini provider boots with a real api_key
// resolved from the environment.
func TestGemini_Boot(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-gemini.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertBooted("nexus.llm.gemini", "nexus.agent.react")
}

// TestGemini_LiveSmoke validates a single round-trip against the live Gemini
// API including assistant→model role mapping.
func TestGemini_LiveSmoke(t *testing.T) {
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-gemini.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertEventEmitted("llm.response")
	h.AssertEventEmitted("agent.turn.end")

	var found bool
	for _, e := range h.Events() {
		if e.Type != "io.output" {
			continue
		}
		if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "assistant" {
			if strings.Contains(strings.ToUpper(out.Content), "GEMINI OK") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected assistant output to contain 'GEMINI OK'")
	}
}
