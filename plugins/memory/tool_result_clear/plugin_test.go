package tool_result_clear

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newPluginForTest wires an empty bus + default config so each test can
// drive the plugin via raw Emit calls without going through full Init.
func newPluginForTest(t *testing.T) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:                bus,
		logger:             slog.Default(),
		enabled:            true,
		ageTurns:           5,
		sizeBytesThreshold: 1000,
		preserveKinds:      map[string]bool{"error": true, "user_question": true},
		dropStrategy:       dropReplaceEnvelope,
		calls:              make(map[string]*callRecord),
		argIndex:           make(map[string][]string),
		preCleared:         make(map[string]string),
	}
	p.unsubs = append(p.unsubs,
		bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(60)),
		bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(60)),
		bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithPriority(60)),
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(12)),
	)
	return p, bus
}

func TestClearsLargeAgedResults(t *testing.T) {
	p, bus := newPluginForTest(t)

	// turn 0: invoke + result (1500 bytes, big enough to qualify).
	big := strings.Repeat("a", 1500)
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t1", Name: "web_fetch", Arguments: map[string]any{"url": "https://x"}})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t1", Name: "web_fetch", Output: big})

	// Advance 6 turns past age threshold.
	for i := 0; i < 6; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}

	cleared := 0
	bus.Subscribe("memory.tool_result_cleared", func(_ engine.Event[any]) { cleared++ })

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "tool", ToolCallID: "t1", Content: big},
	}}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected 1 cleared event, got %d", cleared)
	}
	if !strings.Contains(req.Messages[0].Content, `cleared="true"`) {
		t.Fatalf("expected envelope marker, got %q", req.Messages[0].Content)
	}

	// Plugin must keep the call/result envelope so the agent retains
	// the fact-of-the-call.
	if req.Messages[0].ToolCallID != "t1" {
		t.Fatalf("ToolCallID lost on clear")
	}
	_ = p
}

func TestPreservesRecentResultsUnderAge(t *testing.T) {
	p, bus := newPluginForTest(t)

	big := strings.Repeat("b", 2000)
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t2", Name: "web_fetch", Arguments: map[string]any{"url": "https://y"}})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t2", Name: "web_fetch", Output: big})
	// Only one turn passes — under age cutoff of 5.
	bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})

	cleared := 0
	bus.Subscribe("memory.tool_result_cleared", func(_ engine.Event[any]) { cleared++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "tool", ToolCallID: "t2", Content: big},
	}}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("did not expect any clear, got %d", cleared)
	}
	if req.Messages[0].Content != big {
		t.Fatalf("body should be intact when under age")
	}
	_ = p
}

func TestPreservesErrorsRegardlessOfAge(t *testing.T) {
	_, bus := newPluginForTest(t)

	big := strings.Repeat("e", 1500)
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t3", Name: "shell"})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t3", Name: "shell", Error: big})
	for i := 0; i < 8; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}

	cleared := 0
	bus.Subscribe("memory.tool_result_cleared", func(_ engine.Event[any]) { cleared++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "tool", ToolCallID: "t3", Content: "Error: " + big},
	}}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("error result must not be cleared, got %d", cleared)
	}
}

func TestSubsequentCallDropsEarlier(t *testing.T) {
	_, bus := newPluginForTest(t)

	body := strings.Repeat("c", 500) // size below threshold, but subsequent-call should still drop.
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t4", Name: "search", Arguments: map[string]any{"q": "go"}})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t4", Name: "search", Output: body})
	bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t5", Name: "search", Arguments: map[string]any{"q": "go"}})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t5", Name: "search", Output: body})
	bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})

	cleared := 0
	bus.Subscribe("memory.tool_result_cleared", func(_ engine.Event[any]) { cleared++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Messages: []events.Message{
		{Role: "tool", ToolCallID: "t4", Content: body},
		{Role: "tool", ToolCallID: "t5", Content: body},
	}}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected first of two duplicate calls to be cleared, got %d", cleared)
	}
	if !strings.Contains(req.Messages[0].Content, `cleared="true"`) {
		t.Fatalf("first message should be cleared, got %q", req.Messages[0].Content)
	}
	if req.Messages[1].Content != body {
		t.Fatalf("latest call body must be preserved, got %q", req.Messages[1].Content)
	}
}

func TestSkipsSourcedRequests(t *testing.T) {
	_, bus := newPluginForTest(t)
	big := strings.Repeat("a", 2000)
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "t6", Name: "web_fetch"})
	bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: "t6", Name: "web_fetch", Output: big})
	for i := 0; i < 8; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}

	cleared := 0
	bus.Subscribe("memory.tool_result_cleared", func(_ engine.Event[any]) { cleared++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Metadata: map[string]any{"_source": "another.plugin"},
		Messages: []events.Message{{Role: "tool", ToolCallID: "t6", Content: big}}}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("must not touch sourced requests, got %d", cleared)
	}
}

func TestEnvelopeMarkerStable(t *testing.T) {
	got := envelopeMarker("abc", "web_fetch", 1000, 5)
	want := `<tool_result id="abc" tool="web_fetch" cleared="true" original_size="1000" cleared_at="turn-5" />`
	if got != want {
		t.Fatalf("envelope mismatch:\n got:  %s\n want: %s", got, want)
	}
}

func TestCanonicalArgHashOrderInvariant(t *testing.T) {
	a := canonicalArgHash(map[string]any{"x": 1, "y": "two"})
	b := canonicalArgHash(map[string]any{"y": "two", "x": 1})
	if a != b {
		t.Fatalf("hash should be order-invariant; got %s vs %s", a, b)
	}
}
