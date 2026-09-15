//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestLLMResponseVetoPassthrough_RefusalSurvivesOutputGate proves the refusal
// reaches the user even when a blocking output-side gate would otherwise veto
// it.
//
// stop_words vetoes at before:llm.response; its refusal message contains an
// internal IP, and content_safety runs at before:io.output in block mode with
// check_ip_internal on. Without the substitution marker riding onto the
// AgentOutput, content_safety vetoes the refusal and the ReAct loop emits no
// assistant output — leaving the user with nothing, which is exactly the
// outcome the substitute exists to prevent.
func TestLLMResponseVetoPassthrough_RefusalSurvivesOutputGate(t *testing.T) {
	h := testharness.New(t, "configs/test-llm-response-veto-passthrough.yaml",
		testharness.WithTimeout(20*time.Second))
	h.Run()

	collected := h.Events()

	// The response was substituted and stamped.
	var substituted *events.LLMResponse
	for _, e := range collected {
		resp, ok := e.Payload.(events.LLMResponse)
		if e.Type != "llm.response" || !ok {
			continue
		}
		if engine.IsVetoSubstituted(resp.Metadata) {
			r := resp
			substituted = &r
		}
	}
	if substituted == nil {
		t.Fatal("no substituted llm.response — the before:llm.response veto did not fire")
	}
	if substituted.FinishReason != engine.FinishReasonVetoed {
		t.Errorf("FinishReason = %q, want %q", substituted.FinishReason, engine.FinishReasonVetoed)
	}

	// The refusal reached the user despite content_safety blocking on the
	// internal IP it contains.
	var assistantOutputs []string
	for _, e := range collected {
		if e.Type != "io.output" {
			continue
		}
		out, ok := e.Payload.(events.AgentOutput)
		if !ok || out.Role != "assistant" {
			continue
		}
		assistantOutputs = append(assistantOutputs, out.Content)
	}
	if len(assistantOutputs) == 0 {
		t.Fatal("no assistant io.output — the output gate vetoed the refusal into a blank")
	}

	sawRefusal := false
	for _, got := range assistantOutputs {
		if strings.Contains(got, "Blocked by policy") {
			sawRefusal = true
		}
		if strings.Contains(got, "forbidden") {
			t.Errorf("banned model text reached the user: %q", got)
		}
	}
	if !sawRefusal {
		t.Errorf("refusal did not survive the output gate; assistant outputs = %q", assistantOutputs)
	}

	// The marker must actually be on the output — that is the mechanism under
	// test, not an incidental pass.
	markerSeen := false
	for _, e := range collected {
		if e.Type != "io.output" {
			continue
		}
		if out, ok := e.Payload.(events.AgentOutput); ok && engine.IsVetoSubstituted(out.Metadata) {
			markerSeen = true
		}
	}
	if !markerSeen {
		t.Error("no io.output carried the substitution marker — the agent loop did not propagate it")
	}
}
