package react

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestHandleToolResult_DropsInternalCalls proves the react agent does not
// decrement pendingToolCalls on results that belong to script-dispatched
// sub-calls (run_code's inner `code-*` ids). Conversation-history filtering
// is tested separately in plugins/memory/capped/plugin_test.go — this
// test only covers the pending-count invariant that keeps an outer turn
// from short-circuiting when an inner result arrives first.
func TestHandleToolResult_DropsInternalCalls(t *testing.T) {
	p := New().(*Plugin)
	p.currentTurnID = "turn-xyz"
	// Two outstanding top-level calls — only the external result should
	// decrement the counter.
	p.pendingToolCalls = 2

	// Inner call from a run_code script — flagged internal by the agent.
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

	// Inner result arrives first — must be dropped without counting.
	p.handleToolResult(events.ToolResult{
		ID:     "code-1-abc",
		Name:   "read_file",
		Output: "inner payload",
		TurnID: "turn-xyz",
	})
	// Top-level result — counts as one.
	p.handleToolResult(events.ToolResult{
		ID:     "toolu_real",
		Name:   "write_file",
		Output: "outer payload",
		TurnID: "turn-xyz",
	})

	if p.pendingToolCalls != 1 {
		t.Errorf("pendingToolCalls = %d, want 1 (inner result must not decrement)", p.pendingToolCalls)
	}
	if _, still := p.internalCallIDs["code-1-abc"]; still {
		t.Error("internalCallIDs should drop id after matching result")
	}
}
