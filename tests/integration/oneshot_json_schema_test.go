//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestOneshotJsonSchema_ValidOutput validates that the agent produces valid
// JSON matching the required schema when the json_schema gate is active.
func TestOneshotJsonSchema_ValidOutput(t *testing.T) {
	h := testharness.New(t, "configs/test-oneshot-json-schema.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertEventEmitted("io.output")

	// Extract assistant output and validate JSON structure.
	output := firstAssistantOutput(t, h)
	if output == "" {
		t.Fatal("no assistant output produced")
	}

	var result struct {
		Summary   string   `json:"summary"`
		Sentiment string   `json:"sentiment"`
		Topics    []string `json:"topics"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, output)
	}

	if result.Summary == "" {
		t.Error("summary field is empty")
	}

	validSentiments := map[string]bool{
		"positive": true, "negative": true, "neutral": true, "mixed": true,
	}
	if !validSentiments[result.Sentiment] {
		t.Errorf("sentiment %q not in valid set (positive/negative/neutral/mixed)", result.Sentiment)
	}

	if len(result.Topics) == 0 {
		t.Error("topics array is empty")
	}
}

// TestOneshotJsonSchema_ValidSchemaPassthrough validates the json_schema gate
// passes valid JSON that conforms to the schema. Uses mock response.
func TestOneshotJsonSchema_ValidSchemaPassthrough(t *testing.T) {
	validJSON := `{"summary":"Great product review","sentiment":"positive","topics":["quality","shipping"]}`

	cfg := copyConfig(t, "configs/test-oneshot-json-schema.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"I love this! Amazing product."},
			"approval_mode": "approve",
			"timeout":       "15s",
			"mock_responses": []map[string]any{
				{"content": validJSON},
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	output := firstAssistantOutput(t, h)
	var result struct {
		Summary   string   `json:"summary"`
		Sentiment string   `json:"sentiment"`
		Topics    []string `json:"topics"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, output)
	}
	if result.Sentiment != "positive" {
		t.Errorf("expected sentiment=positive, got %q", result.Sentiment)
	}
	if len(result.Topics) != 2 {
		t.Errorf("expected 2 topics, got %d", len(result.Topics))
	}
}

// TestOneshotJsonSchema_InvalidSchemaRetry validates the json_schema gate
// triggers retry when output doesn't match schema. Uses mock responses:
// first response is invalid, second is valid — gate should retry and pass.
func TestOneshotJsonSchema_InvalidSchemaRetry(t *testing.T) {
	invalidJSON := `{"summary":"Missing required fields"}`
	validJSON := `{"summary":"Fixed response","sentiment":"negative","topics":["test"]}`

	cfg := copyConfig(t, "configs/test-oneshot-json-schema.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"Terrible experience."},
			"approval_mode": "approve",
			"timeout":       "15s",
			"mock_responses": []map[string]any{
				{"content": invalidJSON},
				{"content": validJSON},
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	output := firstAssistantOutput(t, h)
	var result struct {
		Sentiment string `json:"sentiment"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("final output is not valid JSON: %v\noutput: %s", err, output)
	}
	if result.Sentiment != "negative" {
		t.Errorf("expected sentiment=negative, got %q", result.Sentiment)
	}
}

// firstAssistantOutput extracts the first assistant-role output content.
// It strips any markdown code fences the LLM may wrap around JSON.
func firstAssistantOutput(t *testing.T, h *testharness.Harness) string {
	t.Helper()
	for _, e := range h.Events() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "assistant" {
				content := strings.TrimSpace(out.Content)
				// Strip markdown code fences if present.
				content = strings.TrimPrefix(content, "```json")
				content = strings.TrimPrefix(content, "```")
				content = strings.TrimSuffix(content, "```")
				return strings.TrimSpace(content)
			}
		}
	}
	return ""
}
