package compaction

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestToolResult_SkipsInternalCalls mirrors the conversation plugin test.
// Both memory archivers feed LLM history; both must strip ToolCall entries
// whose ParentCallID!="" so the provider never sees tool_use_ids it didn't
// generate (e.g. run_code's code-* inner calls).
func TestToolResult_SkipsInternalCalls(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.persist = false

	// Outer LLM-facing call.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID:   "outer-1",
		Name: "run_code",
	}})
	// Inner script-dispatched call.
	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID:           "code-inner-1",
		Name:         "read_file",
		ParentCallID: "outer-1",
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID:     "code-inner-1",
		Name:   "read_file",
		Output: "file contents",
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID:     "outer-1",
		Name:   "run_code",
		Output: "done",
	}})

	if n := len(p.messages); n != 2 {
		t.Fatalf("expected 2 archived messages (outer invoke + outer result), got %d: %+v", n, p.messages)
	}
	if got := p.messages[0].ToolCallID; got != "outer-1" {
		t.Errorf("archived msg[0] should be outer-1, got %q", got)
	}
	if got := p.messages[1].ToolCallID; got != "outer-1" {
		t.Errorf("archived msg[1] should be outer-1, got %q", got)
	}
	if len(p.internalCallIDs) != 0 {
		t.Errorf("internalCallIDs should be empty after matched result, got %v", p.internalCallIDs)
	}
}
