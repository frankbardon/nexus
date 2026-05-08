//go:build integration

package integration

// Multi-gate stacking integration test (T3.6).
//
// DEVIATION FROM SPEC: the plan calls for nexus.io.oneshot, but the
// mock_responses queue used to drive the schema-fail-then-pass sequence is a
// nexus.io.test feature, and pkg/testharness only supports nexus.io.test.
// The point of this test is the gate stack and the json_schema retry path,
// not the IO transport, so we use nexus.io.test here. configs/test-oneshot-
// multi-gate.yaml carries the stacked gate configuration.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
	testio "github.com/frankbardon/nexus/plugins/io/test"
)

// TestOneshotMultiGate_Boot validates that all four gates plus the agent
// boot together without conflicts.
func TestOneshotMultiGate_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-oneshot-multi-gate.yaml",
		testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.gate.json_schema",
		"nexus.gate.output_length",
		"nexus.gate.prompt_injection",
		"nexus.gate.content_safety",
		"nexus.agent.react",
		"nexus.io.test",
	)
}

// TestOneshotMultiGate_RetryThenPass exercises the full stacked-gate flow:
//  1. First mock LLM response fails the json_schema gate (missing required
//     fields); other gates (output_length, content_safety) all see the same
//     output but do not veto it.
//  2. The json_schema gate emits a retry llm.request tagged with
//     _source = "nexus.gate.json_schema/retry-..." metadata.
//  3. The mock IO plugin answers the retry with the second mock response,
//     which conforms to the schema.
//  4. Final io.output (assistant role) carries the corrected JSON.
//  5. No system errors propagate.
func TestOneshotMultiGate_RetryThenPass(t *testing.T) {
	h := testharness.New(t, "configs/test-oneshot-multi-gate.yaml",
		testharness.WithTimeout(20*time.Second))
	h.Run()

	collected := h.Events()

	// --- Assertion 1: the json_schema gate emitted at least one retry
	// llm.request. The retrier tags every retry with _source prefixed by
	// the gate's plugin ID and Tags["source_plugin"] set the same way.
	// The agent's primary request was vetoed at before:llm.request by the
	// mock IO plugin, so the only llm.request events on the bus are the
	// gate retries.
	llmReqs := collectLLMRequests(collected)
	if !hasJSONSchemaRetrySource(llmReqs) {
		// Diagnostic dump on failure.
		for i, e := range collected {
			t.Logf("[%03d] type=%s source=%s payload=%+v", i, e.Type, e.Source, e.Payload)
		}
		t.Fatalf("expected an llm.request with _source prefixed by %q, got %d llm.request events with sources=%v",
			"nexus.gate.json_schema", len(llmReqs), llmRequestSources(llmReqs))
	}

	// --- Assertion 2: both mock responses surfaced on the bus — the
	// invalid first-pass and the corrected retry response. Order on the
	// wildcard channel is not deterministic (the gate's synchronous
	// retry response can land before the goroutine-emitted original),
	// so we just verify both contents are present.
	llmResps := collectLLMResponses(collected)
	sawInvalid := anyResponseMatches(llmResps, looksLikeInvalidSchemaResp)
	sawValid := anyResponseMatches(llmResps, func(r events.LLMResponse) bool {
		return strings.Contains(r.Content, `"sentiment":"positive"`) &&
			strings.Contains(r.Content, `"topics"`)
	})
	if !sawInvalid {
		t.Errorf("expected an llm.response carrying the invalid mock payload; collected=%d",
			len(llmResps))
	}
	if !sawValid {
		t.Errorf("expected an llm.response carrying the valid corrected payload; collected=%d",
			len(llmResps))
	}

	// --- Assertion 3: the corrected retry response specifically carries
	// the json_schema gate's _source metadata. This proves the retry
	// path drove the corrected content (not, say, a stray repeat of the
	// original mock).
	retryRespFound := false
	for _, r := range llmResps {
		src, _ := r.Metadata["_source"].(string)
		if strings.HasPrefix(src, "nexus.gate.json_schema") &&
			strings.Contains(r.Content, `"sentiment":"positive"`) {
			retryRespFound = true
			break
		}
	}
	if !retryRespFound {
		t.Errorf("expected a corrected llm.response tagged with json_schema retry _source; got responses=%v",
			llmResponseSources(llmResps))
	}

	// --- Assertion 4: final assistant io.output carries the corrected
	// JSON. The json_schema gate updates output content in-place when
	// the retry succeeds, so the assistant-role io.output the user sees
	// is the corrected payload — not the invalid first-pass.
	finalOutput := lastAssistantOutput(t, h)
	if finalOutput == "" {
		t.Fatal("no assistant io.output collected")
	}
	var parsed struct {
		Summary   string   `json:"summary"`
		Sentiment string   `json:"sentiment"`
		Topics    []string `json:"topics"`
	}
	if err := json.Unmarshal([]byte(finalOutput), &parsed); err != nil {
		t.Fatalf("final assistant output is not valid JSON: %v\noutput: %s", err, finalOutput)
	}
	if parsed.Sentiment != "positive" {
		t.Errorf("expected corrected payload sentiment=positive, got %q (output=%s)",
			parsed.Sentiment, finalOutput)
	}
	if len(parsed.Topics) == 0 {
		t.Errorf("expected non-empty topics in corrected payload, got %v", parsed.Topics)
	}
	if parsed.Summary == "" {
		t.Errorf("expected non-empty summary in corrected payload")
	}

	// --- Assertion 5: no gate produced a system-role io.output. Such an
	// event would mean a gate vetoed (content_safety block, json_schema
	// retries-exhausted, prompt_injection block, etc.) — none of which
	// should fire in this happy path. A "system" or "error" role would
	// indicate a gate or system failure leaked to the user.
	//
	// NOTE: a core.error from the Anthropic provider IS expected in mock
	// mode — the retry path emits llm.request directly (bypassing the
	// before:llm.request veto chain), so the live provider also picks
	// it up and 401s on the mock api_key. This is the same pattern used
	// by oneshot_json_schema_test.go's InvalidSchemaRetry. The
	// resulting system-role io.output for the error is intentionally
	// not asserted against.
	systemOutputs := 0
	for _, e := range collected {
		if e.Type != "io.output" {
			continue
		}
		out, ok := e.Payload.(events.AgentOutput)
		if !ok {
			continue
		}
		if out.Role == "system" {
			systemOutputs++
			t.Errorf("unexpected system-role io.output (gate veto leaked): %s", out.Content)
		}
	}
	_ = systemOutputs
}

// -- helpers ---------------------------------------------------------------

func collectLLMRequests(es []testio.CollectedEvent) []events.LLMRequest {
	var out []events.LLMRequest
	for _, e := range es {
		if e.Type != "llm.request" {
			continue
		}
		if req, ok := e.Payload.(events.LLMRequest); ok {
			out = append(out, req)
		}
	}
	return out
}

func collectLLMResponses(es []testio.CollectedEvent) []events.LLMResponse {
	var out []events.LLMResponse
	for _, e := range es {
		if e.Type != "llm.response" {
			continue
		}
		if resp, ok := e.Payload.(events.LLMResponse); ok {
			out = append(out, resp)
		}
	}
	return out
}

func hasJSONSchemaRetrySource(reqs []events.LLMRequest) bool {
	for _, r := range reqs {
		src, _ := r.Metadata["_source"].(string)
		if strings.HasPrefix(src, "nexus.gate.json_schema") {
			return true
		}
	}
	return false
}

func llmRequestSources(reqs []events.LLMRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		src, _ := r.Metadata["_source"].(string)
		if src == "" {
			src = "<none>"
		}
		out = append(out, src)
	}
	return out
}

func llmResponseSources(resps []events.LLMResponse) []string {
	out := make([]string, 0, len(resps))
	for _, r := range resps {
		src, _ := r.Metadata["_source"].(string)
		if src == "" {
			src = "<none>"
		}
		out = append(out, src)
	}
	return out
}

func looksLikeInvalidSchemaResp(r events.LLMResponse) bool {
	// Expect the first invalid mock: only "summary" populated, no
	// "sentiment" or "topics".
	return strings.Contains(r.Content, `"summary"`) &&
		!strings.Contains(r.Content, `"sentiment"`) &&
		!strings.Contains(r.Content, `"topics"`)
}

func anyResponseMatches(resps []events.LLMResponse, pred func(events.LLMResponse) bool) bool {
	for _, r := range resps {
		if pred(r) {
			return true
		}
	}
	return false
}

// lastAssistantOutput returns the last assistant-role io.output content,
// stripping any surrounding markdown fences.
func lastAssistantOutput(t *testing.T, h *testharness.Harness) string {
	t.Helper()
	var content string
	for _, e := range h.Events() {
		if e.Type != "io.output" {
			continue
		}
		out, ok := e.Payload.(events.AgentOutput)
		if !ok {
			continue
		}
		if out.Role == "assistant" {
			c := strings.TrimSpace(out.Content)
			c = strings.TrimPrefix(c, "```json")
			c = strings.TrimPrefix(c, "```")
			c = strings.TrimSuffix(c, "```")
			content = strings.TrimSpace(c)
		}
	}
	return content
}
