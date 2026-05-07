package simple

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestAppendMessages verifies that io.input, llm.response, and tool.result
// all land in the buffer in the expected LLM-native shape.
func TestAppendMessages(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()

	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "hi"}})
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "hello",
		ToolCalls: []events.ToolCallRequest{
			{ID: "t1", Name: "shell", Arguments: `{"cmd":"ls"}`},
		},
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t1",
		Name:   "shell",
		Output: "file.txt",
	}})

	msgs := p.GetHistory()
	if got, want := len(msgs), 3; got != want {
		t.Fatalf("len(history) = %d, want %d: %+v", got, want, msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("msg[0] = %+v, want user/hi", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msg[1] = %+v, want assistant with 1 tool call", msgs[1])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "t1" || msgs[2].Content != "file.txt" {
		t.Errorf("msg[2] = %+v, want tool/t1/file.txt", msgs[2])
	}
}

// TestInternalCallsFiltered proves that tool results for ParentCallID-flagged
// calls never reach history. Same invariant as the capped plugin.
func TestInternalCallsFiltered(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()

	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "code-inner-1",
		Name:         "discover",
		ParentCallID: "outer-1",
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "code-inner-1",
		Name:   "discover",
		Output: "dropped",
	}})

	if got := len(p.GetHistory()); got != 0 {
		t.Fatalf("expected history empty, got %d messages", got)
	}
}

// TestHistoryQuery proves the synchronous memory.history.query contract:
// handler mutates the pointer in place.
func TestHistoryQuery(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "hi"}})

	q := &events.HistoryQuery{SchemaVersion: events.HistoryQueryVersion}
	p.handleHistoryQuery(engine.Event[any]{Payload: q})
	if got := len(q.Messages); got != 1 {
		t.Fatalf("q.Messages len = %d, want 1", got)
	}
	if q.Messages[0].Role != "user" {
		t.Errorf("q.Messages[0].Role = %q, want user", q.Messages[0].Role)
	}
}

// TestCompactedReplacesBuffer verifies memory.compacted replaces the buffer.
func TestCompactedReplacesBuffer(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()
	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "one"}})
	p.handleInput(engine.Event[any]{Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "two"}})

	p.handleCompacted(engine.Event[any]{Payload: events.CompactionComplete{SchemaVersion: events.CompactionCompleteVersion, Messages: []events.Message{{Role: "system", Content: "summary"}}}})

	msgs := p.GetHistory()
	if len(msgs) != 1 || msgs[0].Content != "summary" {
		t.Fatalf("buffer not replaced: %+v", msgs)
	}
}

// TestLLMResponseWithSourceIgnored proves that llm.response events tagged
// with Metadata["_source"] (e.g. planner or summariser replies) don't land
// in the history buffer.
func TestLLMResponseWithSourceIgnored(t *testing.T) {
	p := New().(*Plugin)
	p.logger = slog.Default()

	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "internal",
		Metadata: map[string]any{"_source": "nexus.planner.dynamic"},
	}})

	if got := len(p.GetHistory()); got != 0 {
		t.Fatalf("expected sourced response ignored, got %d messages", got)
	}
}
