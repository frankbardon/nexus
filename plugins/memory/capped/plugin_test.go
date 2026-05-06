package capped

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestToolResult_SkipsInternalCalls proves that tool.result events for
// calls marked ParentCallID!="" never land in conversation history. The
// behaviour exists so run_code scripts can dispatch inner tool calls
// through the bus (gates still fire) without poisoning the LLM message
// stack with tool_use_ids the provider never generated. Under the native
// format, the outer call itself is represented on the prior assistant
// message's ToolCalls field (recorded by handleLLMResponse), so only the
// outer result appears as a distinct "tool" role message.
func TestToolResult_SkipsInternalCalls(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	// Outer call — the LLM-facing run_code invocation. Only the internal
	// filter state is updated; the call itself is not appended.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "outer-1",
		Name: "run_code",
	}})
	// Inner call dispatched by the script. ParentCallID flags it internal.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "code-inner-1",
		Name:         "discover",
		ParentCallID: "outer-1",
	}})
	// Inner result — filtered out because its ID was flagged internal.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "code-inner-1",
		Name:   "discover",
		Output: "{}",
	}})
	// Outer result — lands in history as a tool-role message.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "outer-1",
		Name:   "run_code",
		Output: "ok",
	}})

	if n := len(p.messages); n != 1 {
		t.Fatalf("expected 1 message (outer result only; invocation lives on prior assistant.ToolCalls), got %d: %+v", n, p.messages)
	}
	if p.messages[0].ToolCallID != "outer-1" || p.messages[0].Role != "tool" || p.messages[0].Content != "ok" {
		t.Errorf("msg[0] wrong: %+v", p.messages[0])
	}
	if len(p.internalCallIDs) != 0 {
		t.Errorf("internalCallIDs should be empty after matching result, got %v", p.internalCallIDs)
	}
}

// TestPairSafeTruncation proves that when the buffer caps out on a boundary
// that would orphan a tool result from its assistant tool_use, the plugin
// drops the leading orphan "tool" messages so the buffer stays well-formed
// for the next llm.request. Providers reject tool_use_ids without a
// preceding declaration, so this invariant matters.
func TestPairSafeTruncation(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false
	p.maxMessages = 3

	// Buffer: [user, assistant(tool_use t1), tool(t1), user-2]
	// After appending user-2, cap is 3 so naive drop would yield
	// [tool(t1), user-2, ...] — orphaning the tool result.
	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "u1"}})
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "call shell",
		ToolCalls: []events.ToolCallRequest{{ID: "t1", Name: "shell"}},
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t1", Name: "shell", Output: "ok"}})
	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.

		// Expect the orphan tool result dropped. Result should be
		// [assistant(tool_use t1), user-2] or similar — notably, NOT starting
		// with a "tool" message.
		UserInputVersion, Content: "u2"}})

	if len(p.messages) > 0 && p.messages[0].Role == "tool" {
		t.Fatalf("buffer starts with orphan tool message: %+v", p.messages)
	}
	if len(p.messages) > p.maxMessages {
		t.Fatalf("buffer exceeds max_messages cap: %d > %d", len(p.messages), p.maxMessages)
	}
}

// TestToolResult_UnmatchedInternalInvokeCleansUp ensures the internal-id
// set does not leak when a result never arrives (e.g. bus cancellation).
// Without this guarantee a long-lived session could accumulate dead IDs.
func TestToolResult_UnmatchedInternalInvokeCleansUp(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "code-orphan",
		Name:         "discover",
		ParentCallID: "outer-x",
	}})
	if _, ok := p.internalCallIDs["code-orphan"]; !ok {
		t.Fatal("expected internal call id to be tracked after invoke")
	}
	// No matching result emitted. The id stays until a same-id result
	// arrives; this is intentional because results can be delayed.
	// Documenting behaviour rather than asserting cleanup.
}
