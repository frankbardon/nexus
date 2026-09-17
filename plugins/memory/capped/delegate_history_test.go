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

// TestToolResult_SkipsSubSessionTurns is the other half of the delegate leak.
// Skipping the sub-agent's llm.response keeps its assistant turn out of the
// parent's history, but the sub-agent's tools are dispatched on the SAME bus
// (pkg/delegate.invokeTools emits tool.invoke with the sub-session's TurnID),
// and this plugin subscribes tool.result unconditionally. The sub-agent's
// result therefore landed in the parent's buffer as a tool-role message whose
// ToolCallID no assistant message in that conversation ever declared — which
// the Gemini provider maps to a functionResponse naming a function the request
// never called (convertMessages falls back to the raw call id when it has no
// name for it).
//
// The filter is on TurnID rather than on ParentCallID, and that choice is
// load-bearing: pkg/delegate is also entered from plugins/workflows/icm with
// no parent tool call in scope at all, so a ParentCallID stamped at the
// `delegate` tool would have closed one entry point and left the others open —
// excluding by enumeration, which is the failure this whole area came from.
func TestToolResult_SkipsSubSessionTurns(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	const parentTurn = "turn-parent"
	const subTurn = "delegate_sub1" // pkg/delegate: "delegate_" + subSessionID

	p.handleInput(engine.Event[any]{Payload: events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "who leads athletic apparel?",
	}})
	// The parent asks for the delegate tool. This is the call the
	// conversation declared, and its TurnID is the conversation's.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		ToolCalls:     []events.ToolCallRequest{{ID: "call-delegate", Name: "delegate"}},
		Metadata:      map[string]any{"task_kind": "react_main", "_source": "nexus.agent.react"},
	}})
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-delegate",
		Name:          "delegate",
		TurnID:        parentTurn,
	}})
	// Inside the sub-session. The response is skipped by task_kind; the tool
	// call and its result are not, and carry the sub-session's turn.
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		ToolCalls:     []events.ToolCallRequest{{ID: "call-sub", Name: "run_query"}},
		Metadata:      map[string]any{"task_kind": "delegate", "_source": "delegate.sub1", "posture": "data"},
	}})
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-sub",
		Name:          "run_query",
		TurnID:        subTurn,
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-sub",
		Name:          "run_query",
		Output:        `{"rows":[{"brand_id":6,"beraScore":97.9}]}`,
		TurnID:        subTurn,
	}})
	// The delegate tool returns to the parent, on the parent's turn.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-delegate",
		Name:          "delegate",
		Output:        "nike leads",
		TurnID:        parentTurn,
	}})

	// The invariant: no tool-role message may carry a ToolCallID that no
	// assistant message in this conversation declared.
	for i, msg := range p.messages {
		if msg.Role != "tool" {
			continue
		}
		if !p.declaresToolCall(msg.ToolCallID) {
			t.Fatalf("messages[%d] is a tool result for %q, which no assistant turn in this "+
				"conversation declared. history: %+v", i, msg.ToolCallID, p.messages)
		}
	}
	// Stated directly as well, so the failure names the sub-agent rather than
	// only the invariant it broke.
	for _, msg := range p.messages {
		if msg.ToolCallID == "call-sub" {
			t.Fatalf("sub-session tool RESULT leaked into the top-level history: %+v", p.messages)
		}
	}
	// And the parent's own result is still recorded — the filter must narrow,
	// not empty, the buffer.
	var sawDelegate bool
	for _, msg := range p.messages {
		if msg.Role == "tool" && msg.ToolCallID == "call-delegate" && msg.Content == "nike leads" {
			sawDelegate = true
		}
	}
	if !sawDelegate {
		t.Fatalf("the delegate tool's own result should be recorded; history: %+v", p.messages)
	}
}

// TestToolResult_TurnFilterInertWithoutADeclaredCall pins the deliberately
// inert case. A buffer that has never recorded a tool-calling assistant turn
// has no way to tell whose turn a result belongs to, and an engine caller may
// drive the tool bus with a TurnID of its own choosing; guessing there would
// drop real results. nexus.agent.react's equivalent guard is inert on the same
// terms.
func TestToolResult_TurnFilterInertWithoutADeclaredCall(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	// No assistant message declared anything, so nothing is learned.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "seeded-1",
		Name:          "scene_create",
		TurnID:        "some-other-turn",
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "seeded-1",
		Name:          "scene_create",
		Output:        "ok",
		TurnID:        "some-other-turn",
	}})

	if n := len(p.messages); n != 1 {
		t.Fatalf("expected the result to be recorded while no turn is known; got %d: %+v", n, p.messages)
	}
	if p.conversationTurnID != "" {
		t.Fatalf("conversationTurnID should stay empty when no assistant turn declared the call; got %q", p.conversationTurnID)
	}
}
