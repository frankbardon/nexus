package react

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestHandleToolResult_DropsInternalCalls proves the react agent never
// stitches script-dispatched tool_use_ids into its history. Without this
// filter, Anthropic rejects the next request because run_code's inner
// `code-*` ids have no matching tool_use block in the assistant message.
func TestHandleToolResult_DropsInternalCalls(t *testing.T) {
	p := New().(*Plugin)
	p.currentTurnID = "turn-xyz"
	// Two outstanding top-level calls — only the external result should
	// decrement the counter.
	p.pendingToolCalls = 2

	// Inner call from a run_code script.
	p.handleToolInvokeEvent(engine.Event[any]{Payload: events.ToolCall{
		ID:           "code-1-abc",
		Name:         "read_file",
		TurnID:       "turn-xyz",
		ParentCallID: "toolu_outer",
	}})
	// Top-level call from the LLM.
	p.handleToolInvokeEvent(engine.Event[any]{Payload: events.ToolCall{
		ID:     "toolu_real",
		Name:   "write_file",
		TurnID: "turn-xyz",
	}})

	// Inner result arrives first — must be dropped.
	p.handleToolResult(events.ToolResult{
		ID:     "code-1-abc",
		Name:   "read_file",
		Output: "inner payload",
		TurnID: "turn-xyz",
	})
	// Top-level result — must land in history.
	p.handleToolResult(events.ToolResult{
		ID:     "toolu_real",
		Name:   "write_file",
		Output: "outer payload",
		TurnID: "turn-xyz",
	})

	if len(p.history) != 1 {
		t.Fatalf("expected exactly 1 history entry (toolu_real), got %d: %+v", len(p.history), p.history)
	}
	if got := p.history[0].ToolCallID; got != "toolu_real" {
		t.Errorf("history[0] ToolCallID = %q, want toolu_real", got)
	}
	if p.pendingToolCalls != 1 {
		t.Errorf("pendingToolCalls = %d, want 1 (inner result must not decrement)", p.pendingToolCalls)
	}
	if _, still := p.internalCallIDs["code-1-abc"]; still {
		t.Error("internalCallIDs should drop id after matching result")
	}
}
