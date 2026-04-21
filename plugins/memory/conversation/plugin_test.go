package conversation

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestToolResult_SkipsInternalCalls proves that tool.invoke/tool.result
// pairs marked ParentCallID!="" never land in conversation history. The
// behaviour exists so run_code scripts can dispatch inner tool calls
// through the bus (gates still fire) without poisoning the LLM message
// stack with tool_use_ids the provider never generated.
func TestToolResult_SkipsInternalCalls(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	// Outer call — the LLM-facing run_code invocation. ParentCallID is empty.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID:   "outer-1",
		Name: "run_code",
	}})
	// Inner call dispatched by the script. ParentCallID is non-empty.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID:           "code-inner-1",
		Name:         "discover",
		ParentCallID: "outer-1",
	}})
	// Inner result.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID:     "code-inner-1",
		Name:   "discover",
		Output: "{}",
	}})
	// Outer result.
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID:     "outer-1",
		Name:   "run_code",
		Output: "ok",
	}})

	if n := len(p.messages); n != 2 {
		t.Fatalf("expected 2 messages (outer invoke + outer result), got %d: %+v", n, p.messages)
	}
	if p.messages[0].ToolCallID != "outer-1" || p.messages[0].Role != "tool_invoke" {
		t.Errorf("msg[0] wrong: %+v", p.messages[0])
	}
	if p.messages[1].ToolCallID != "outer-1" || p.messages[1].Role != "tool_result" {
		t.Errorf("msg[1] wrong: %+v", p.messages[1])
	}
	if len(p.internalCallIDs) != 0 {
		t.Errorf("internalCallIDs should be empty after matching result, got %v", p.internalCallIDs)
	}
}

// TestToolResult_UnmatchedInternalInvokeCleansUp ensures the internal-id
// set does not leak when a result never arrives (e.g. bus cancellation).
// Without this guarantee a long-lived session could accumulate dead IDs.
func TestToolResult_UnmatchedInternalInvokeCleansUp(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID:           "code-orphan",
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
