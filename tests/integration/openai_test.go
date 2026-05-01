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

// TestOpenAI_Boot validates the OpenAI provider boots with a real api_key
// resolved from the environment.
func TestOpenAI_Boot(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-openai.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertBooted("nexus.llm.openai", "nexus.agent.react")
}

// TestOpenAI_LiveSmoke validates a single round-trip against the live OpenAI
// API: agent emits llm.request, provider streams a response, agent emits
// io.output with assistant content.
func TestOpenAI_LiveSmoke(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	h := testharness.New(t, "configs/test-openai.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertEventEmitted("llm.response")
	h.AssertEventEmitted("agent.turn.end")

	// Look for the expected token in any assistant output.
	var found bool
	for _, e := range h.Events() {
		if e.Type != "io.output" {
			continue
		}
		if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "assistant" {
			if strings.Contains(strings.ToUpper(out.Content), "OPENAI OK") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected assistant output to contain 'OPENAI OK'")
	}
}
