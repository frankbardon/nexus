package summary_buffer

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin wires the plugin against a real in-process bus and records
// every llm.request it emits so tests can assert summarisation behaviour
// without driving the full engine lifecycle.
func newTestPlugin(t *testing.T) (*Plugin, *requestRecorder) {
	t.Helper()
	bus := engine.NewEventBus()
	rec := &requestRecorder{}
	bus.Subscribe("llm.request", rec.record)

	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	return p, rec
}

type requestRecorder struct {
	mu   sync.Mutex
	reqs []events.LLMRequest
}

func (r *requestRecorder) record(e engine.Event[any]) {
	if req, ok := e.Payload.(events.LLMRequest); ok {
		r.mu.Lock()
		r.reqs = append(r.reqs, req)
		r.mu.Unlock()
	}
}

func (r *requestRecorder) last() *events.LLMRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return nil
	}
	out := r.reqs[len(r.reqs)-1]
	return &out
}

// TestAppendRoundTrip verifies input/response/tool_result land in the buffer
// in the expected LLM-native shape.
func TestAppendRoundTrip(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.messageThreshold = 1000 // avoid accidental trigger

	p.handleInput(engine.Event[any]{Payload: events.UserInput{Content: "hi"}})
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		Content:   "hello",
		ToolCalls: []events.ToolCallRequest{{ID: "t1", Name: "shell"}},
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID: "t1", Name: "shell", Output: "ok",
	}})

	msgs := p.GetHistory()
	if len(msgs) != 3 {
		t.Fatalf("len(history) = %d, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[2].ToolCallID != "t1" {
		t.Errorf("shape wrong: %+v", msgs)
	}
}

// TestTriggerFiresLLMRequest proves that exceeding the message threshold
// dispatches an llm.request tagged with our _source marker.
func TestTriggerFiresLLMRequest(t *testing.T) {
	p, rec := newTestPlugin(t)
	p.messageThreshold = 3
	p.maxRecent = 1

	for i := 0; i < 3; i++ {
		p.handleInput(engine.Event[any]{Payload: events.UserInput{Content: "msg"}})
	}

	last := rec.last()
	if last == nil {
		t.Fatal("expected llm.request emitted on threshold trip")
	}
	if src, _ := last.Metadata["_source"].(string); src != llmSource {
		t.Errorf("llm.request _source = %q, want %q", src, llmSource)
	}
	if last.Role != p.modelRole {
		t.Errorf("llm.request role = %q, want %q", last.Role, p.modelRole)
	}
}

// TestFinishSummarisationReplacesBuffer proves that the summariser's reply
// collapses the older slice into a single system message while keeping
// max_recent messages verbatim at the tail.
func TestFinishSummarisationReplacesBuffer(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.messageThreshold = 1000
	p.maxRecent = 2

	// Seed 5 plain user messages — no tool pairs so safeSplit is a no-op.
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		p.handleInput(engine.Event[any]{Payload: events.UserInput{Content: c}})
	}
	p.triggerSummarisation("test")
	p.handleLLMResponse(engine.Event[any]{Payload: events.LLMResponse{
		Content:  "SUMMARY",
		Metadata: map[string]any{"_source": llmSource},
	}})

	msgs := p.GetHistory()
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content == "" {
		t.Errorf("msgs[0] = %+v, want system summary", msgs[0])
	}
	if msgs[1].Content != "d" || msgs[2].Content != "e" {
		t.Errorf("protected tail wrong: %+v, %+v", msgs[1], msgs[2])
	}
}

// TestSafeSplitPreservesToolPair proves safeSplit shifts the boundary left
// when the protected-tail window would otherwise orphan a tool result from
// its invocation on the summarised side.
func TestSafeSplitPreservesToolPair(t *testing.T) {
	msgs := []events.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", ToolCalls: []events.ToolCallRequest{{ID: "t9", Name: "shell"}}},
		{Role: "tool", ToolCallID: "t9", Content: "result"},
	}
	got := safeSplit(msgs, 4)
	if got != 3 {
		t.Fatalf("safeSplit=%d, want 3 (shifted to keep invoke+result together)", got)
	}
}

// TestSafeSplitNoPairLeavesIdx proves safeSplit is a no-op when the tail
// contains no tool results.
func TestSafeSplitNoPairLeavesIdx(t *testing.T) {
	msgs := []events.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
	}
	if got := safeSplit(msgs, 2); got != 2 {
		t.Fatalf("safeSplit=%d, want 2 (no shift needed)", got)
	}
}

// TestInternalCallsFiltered proves tool.results for ParentCallID-flagged
// calls never land in the history, matching the capped/simple invariant.
func TestInternalCallsFiltered(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.messageThreshold = 1000

	p.handleToolInvoke(engine.Event[any]{Payload: events.ToolCall{
		ID: "code-1", Name: "discover", ParentCallID: "outer",
	}})
	p.handleToolResult(engine.Event[any]{Payload: events.ToolResult{
		ID: "code-1", Name: "discover", Output: "dropped",
	}})
	if got := len(p.GetHistory()); got != 0 {
		t.Fatalf("expected empty history, got %d", got)
	}
}

// TestHistoryQueryContract verifies the synchronous query contract.
func TestHistoryQueryContract(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.messageThreshold = 1000
	p.handleInput(engine.Event[any]{Payload: events.UserInput{Content: "hi"}})

	q := &events.HistoryQuery{}
	p.handleHistoryQuery(engine.Event[any]{Payload: q})
	if len(q.Messages) != 1 || q.Messages[0].Content != "hi" {
		t.Fatalf("q.Messages wrong: %+v", q.Messages)
	}
}

// TestEmptyBufferNoOp proves triggerSummarisation on an empty buffer skips
// LLM dispatch.
func TestEmptyBufferNoOp(t *testing.T) {
	p, rec := newTestPlugin(t)
	p.triggerSummarisation("test")
	if rec.last() != nil {
		t.Fatal("expected no llm.request on empty buffer")
	}
}

// TestAllProtectedNoOp proves triggerSummarisation is a no-op when every
// message is inside the max_recent window.
func TestAllProtectedNoOp(t *testing.T) {
	p, rec := newTestPlugin(t)
	p.messageThreshold = 1000
	p.maxRecent = 10
	p.handleInput(engine.Event[any]{Payload: events.UserInput{Content: "only"}})
	p.triggerSummarisation("test")
	if rec.last() != nil {
		t.Fatal("expected no llm.request when all messages protected")
	}
}
