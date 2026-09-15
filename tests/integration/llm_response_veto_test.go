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

// TestLLMResponseVeto_Boot checks the stack comes up with a gate subscribed
// to before:llm.response.
func TestLLMResponseVeto_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-llm-response-veto.yaml",
		testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.gate.stop_words",
		"nexus.tool.shell",
		"nexus.agent.react",
		"nexus.io.test",
	)
}

// TestLLMResponseVeto_DropsToolCalls is the reason the hook exists.
//
// The mock model response names a banned term and asks to run a shell
// command that is on the sandbox allowlist. nexus.gate.stop_words vetoes at
// before:llm.response, and engine.PublishLLMResponse publishes the gate's
// message with ToolCalls cleared. If the veto did not drop the tool calls,
// the ReAct loop would dispatch the shell call and a tool.result would appear
// on the bus.
//
// before:io.output cannot produce this outcome: it fires only after the loop
// has already run the tools.
func TestLLMResponseVeto_DropsToolCalls(t *testing.T) {
	h := testharness.New(t, "configs/test-llm-response-veto.yaml",
		testharness.WithTimeout(20*time.Second))
	h.Run()

	collected := h.Events()

	// --- The shell tool must never have run.
	for _, e := range collected {
		if e.Type != "tool.invoke" && e.Type != "tool.result" {
			continue
		}
		for i, ev := range collected {
			t.Logf("[%03d] type=%s payload=%+v", i, ev.Type, ev.Payload)
		}
		t.Fatalf("%s reached the bus — the veto failed to drop the response's tool calls", e.Type)
	}

	// --- Exactly one llm.response, carrying the substitute.
	var responses []events.LLMResponse
	for _, e := range collected {
		if e.Type != "llm.response" {
			continue
		}
		resp, ok := e.Payload.(events.LLMResponse)
		if !ok {
			continue
		}
		responses = append(responses, resp)
	}
	if len(responses) != 1 {
		t.Fatalf("got %d llm.response events, want exactly 1", len(responses))
	}

	resp := responses[0]
	if len(resp.ToolCalls) != 0 {
		t.Errorf("published response still carries %d tool calls", len(resp.ToolCalls))
	}
	if resp.FinishReason != engine.FinishReasonVetoed {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, engine.FinishReasonVetoed)
	}
	if strings.Contains(resp.Content, "forbidden") {
		t.Errorf("banned text survived into the published response: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "Content blocked") {
		t.Errorf("Content = %q, want the gate's replacement message", resp.Content)
	}
	if vetoed, _ := resp.Metadata[engine.MetaVetoed].(bool); !vetoed {
		t.Error("published response is not stamped with the vetoed metadata flag")
	}

	// --- The refusal must actually reach the user.
	//
	// This is the half a veto-that-only-drops cannot deliver: the caller
	// would be left choosing between a fabricated answer and a blank one.
	// The substitute's Content rides through the agent loop onto io.output,
	// so the operator's message is what the user sees.
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
		t.Fatal("no assistant io.output produced — the vetoed turn appears to have stalled")
	}
	for _, got := range assistantOutputs {
		if strings.Contains(got, "forbidden") {
			t.Errorf("banned text reached the user: %q", got)
		}
	}
	sawReason := false
	for _, got := range assistantOutputs {
		if strings.Contains(got, "Content blocked") {
			sawReason = true
		}
	}
	if !sawReason {
		t.Errorf("refusal never reached the user; assistant outputs = %q", assistantOutputs)
	}
}
