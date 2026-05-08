//go:build integration

package integration

import (
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestToolChoiceCrossProvider validates that the ReAct agent's tool_choice
// sequence reaches each provider's LLM request payload identically — the
// agent-side override semantics are provider-agnostic, so an Anthropic-routed
// run and an OpenAI-routed run see the same ToolChoice values in the same order.
//
// The mocked test IO plugin vetoes before:llm.request before any provider HTTP
// call happens, so we can assert on the request payload that *would* have hit
// the provider without needing real API keys.
//
// Approach: run two subtests with different provider configs (one engine each
// to avoid global-state contention from twice-booting in the same process),
// capture each run's tool_choice sequence, then compare the two traces in a
// dedicated parity assertion.
func TestToolChoiceCrossProvider(t *testing.T) {
	anthropicChoices := runToolChoiceLeg(t, "anthropic",
		"configs/test-tool-choice-cross-provider-anthropic.yaml",
		"nexus.llm.anthropic")

	openaiChoices := runToolChoiceLeg(t, "openai",
		"configs/test-tool-choice-cross-provider-openai.yaml",
		"nexus.llm.openai")

	// Parity: same number of agent-driven LLM requests in both runs.
	if len(anthropicChoices) != len(openaiChoices) {
		t.Fatalf("parity: agent emitted %d LLM requests under anthropic but %d under openai",
			len(anthropicChoices), len(openaiChoices))
	}

	// Parity: each iteration carries the identical ToolChoice in both runs.
	for i := range anthropicChoices {
		a := anthropicChoices[i]
		o := openaiChoices[i]
		switch {
		case a == nil && o == nil:
			// both unset — fine
		case a == nil || o == nil:
			t.Errorf("iter %d: tool_choice presence mismatch (anthropic=%+v openai=%+v)", i+1, a, o)
		case a.Mode != o.Mode || a.Name != o.Name:
			t.Errorf("iter %d: tool_choice mismatch — anthropic={Mode:%q Name:%q} openai={Mode:%q Name:%q}",
				i+1, a.Mode, a.Name, o.Mode, o.Name)
		}
	}

	// The configured sequence is required → tool/read_file → auto. Both legs
	// must have walked the sequence identically.
	if len(anthropicChoices) < 3 {
		t.Fatalf("expected at least 3 agent LLM requests per leg, got %d", len(anthropicChoices))
	}
	if anthropicChoices[0] == nil || anthropicChoices[0].Mode != "required" {
		t.Errorf("iter 1 anthropic: expected mode=required, got %+v", anthropicChoices[0])
	}
	if openaiChoices[0] == nil || openaiChoices[0].Mode != "required" {
		t.Errorf("iter 1 openai: expected mode=required, got %+v", openaiChoices[0])
	}
	if anthropicChoices[1] == nil || anthropicChoices[1].Mode != "tool" || anthropicChoices[1].Name != "read_file" {
		t.Errorf("iter 2 anthropic: expected mode=tool name=read_file, got %+v", anthropicChoices[1])
	}
	if openaiChoices[1] == nil || openaiChoices[1].Mode != "tool" || openaiChoices[1].Name != "read_file" {
		t.Errorf("iter 2 openai: expected mode=tool name=read_file, got %+v", openaiChoices[1])
	}
	for i := 2; i < len(anthropicChoices); i++ {
		if anthropicChoices[i] == nil || anthropicChoices[i].Mode != "auto" {
			t.Errorf("iter %d anthropic: expected mode=auto, got %+v", i+1, anthropicChoices[i])
		}
		if openaiChoices[i] == nil || openaiChoices[i].Mode != "auto" {
			t.Errorf("iter %d openai: expected mode=auto, got %+v", i+1, openaiChoices[i])
		}
	}
}

// runToolChoiceLeg boots one harness, captures the tool_choice on every
// agent-driven before:llm.request, asserts the expected provider booted and
// (since responses are mocked) no real provider HTTP call ever fires, and
// returns the captured sequence for cross-leg comparison.
func runToolChoiceLeg(t *testing.T, label, configPath, expectedProvider string) []*events.ToolChoice {
	t.Helper()

	var choices []*events.ToolChoice
	t.Run(label, func(t *testing.T) {
		h := testharness.New(t, configPath, testharness.WithTimeout(30*time.Second))

		var (
			mu       sync.Mutex
			captured []*events.ToolChoice
		)
		// Priority 15 fires before the mock interceptor at priority 20, so we
		// observe the request with its tool_choice intact. We skip internal
		// sub-requests (planner/summariser/etc.) because only the agent's
		// main loop carries the configured sequence.
		unsub := h.Bus().Subscribe("before:llm.request", func(e engine.Event[any]) {
			vp, ok := e.Payload.(*engine.VetoablePayload)
			if !ok {
				return
			}
			req, ok := vp.Original.(*events.LLMRequest)
			if !ok {
				return
			}
			kind, _ := req.Metadata["task_kind"].(string)
			switch kind {
			case "plan", "summarise", "compact", "classify":
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if req.ToolChoice == nil {
				captured = append(captured, nil)
				return
			}
			cp := *req.ToolChoice
			captured = append(captured, &cp)
		}, engine.WithPriority(15))
		defer unsub()

		h.Run()

		// Confirm the right provider plugin actually booted in this leg.
		h.AssertBooted("nexus.agent.react", expectedProvider)

		mu.Lock()
		choices = make([]*events.ToolChoice, len(captured))
		copy(choices, captured)
		mu.Unlock()

		if len(choices) == 0 {
			t.Fatalf("%s leg: no agent-driven LLM requests captured", label)
		}
	})
	return choices
}
