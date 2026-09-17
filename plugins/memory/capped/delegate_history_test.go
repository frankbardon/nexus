package capped

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestLLMResponse_SkipsDelegateSubSession proves that an llm.response
// produced inside a pkg/delegate sub-session never lands in the top-level
// conversation history. The sub-session runs its own in-process history and
// stamps task_kind "delegate" (pkg/delegate/runtime.go, requestLLM);
// nexus.agent.react already ignored those because it filters on _source, but
// this plugin filters on task_kind and the set omitted "delegate". The five
// pre-existing internal kinds are asserted alongside it so the delegate entry
// reads as one of a set rather than as a special case.
func TestLLMResponse_SkipsDelegateSubSession(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	for _, kind := range []string{"plan", "classify", "summarise", "compact", "subagent", "delegate"} {
		p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "internal output from " + kind,
			ToolCalls:     []events.ToolCallRequest{{ID: "skipped-" + kind, Name: "some_tool"}},
			Metadata:      map[string]any{"task_kind": kind, "_source": "delegate.sub-1"},
		}})
		if n := len(p.messages); n != 0 {
			t.Fatalf("task_kind %q recorded %d message(s) into the top-level history: %+v", kind, n, p.messages)
		}
	}

	// The main agent loop IS the conversation and is still recorded.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "here is the answer",
		Metadata:      map[string]any{"task_kind": "react_main", "_source": "nexus.agent.react"},
	}})
	if n := len(p.messages); n != 1 {
		t.Fatalf("react_main response should be recorded; got %d messages: %+v", n, p.messages)
	}
}

// TestDelegateTurnsNeverStackTwoAssistantMessages replays the shape that broke
// in production: a top-level agent whose only tool is `delegate`, a sub-agent
// that calls a tool of its own, then a final answer. Before the fix the
// history read
//
//	user -> assistant(delegate call) -> assistant(sub-agent tool call) -> ...
//
// and the Gemini provider maps every assistant message to role "model" with
// functionCall parts, so contents[2] was a model turn carrying a functionCall
// immediately after another model turn — 400 INVALID_ARGUMENT on every real
// question.
//
// The invariant asserted here is the provider-agnostic one: no assistant
// message carrying ToolCalls may directly follow another assistant message.
func TestDelegateTurnsNeverStackTwoAssistantMessages(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	// The turn starts.
	p.handleInput(engine.Event[any]{Payload: events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "who leads athletic apparel?",
	}})
	// The top-level agent decides to delegate.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		ToolCalls:     []events.ToolCallRequest{{ID: "call-delegate", Name: "delegate"}},
		Metadata:      map[string]any{"task_kind": "react_main", "_source": "nexus.agent.react"},
	}})
	// Inside the sub-session: the sub-agent asks for a tool of its own and
	// then answers its parent. Both responses carry task_kind "delegate" and
	// neither belongs to the parent's conversation.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		ToolCalls:     []events.ToolCallRequest{{ID: "call-sub", Name: "run_query"}},
		Metadata:      map[string]any{"task_kind": "delegate", "_source": "delegate.sub-1", "posture": "data"},
	}})
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "nike leads",
		Metadata:      map[string]any{"task_kind": "delegate", "_source": "delegate.sub-1", "posture": "data"},
	}})
	// The delegate tool returns to the parent.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-delegate",
		Name:          "delegate",
		Output:        "nike leads",
	}})
	// The parent writes the user-visible answer.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "Nike leads athletic apparel.",
		Metadata:      map[string]any{"task_kind": "react_main", "_source": "nexus.agent.react"},
	}})

	for i, msg := range p.messages {
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		if i > 0 && p.messages[i-1].Role == "assistant" {
			t.Fatalf("messages[%d] is an assistant tool call following another assistant message; "+
				"Gemini rejects that with 400 INVALID_ARGUMENT. history: %+v", i, p.messages)
		}
	}
	// And the sub-agent's own tool call is absent from the parent's history.
	for _, msg := range p.messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID == "call-sub" {
				t.Fatalf("sub-session tool call leaked into the top-level history: %+v", p.messages)
			}
		}
	}
}
