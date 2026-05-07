package tool_def_pruner

import (
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newPluginForTest(t *testing.T) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:                  bus,
		logger:               slog.Default(),
		enabled:              true,
		unusedTurnsThreshold: 3,
		neverPrune:           map[string]bool{"discover": true},
		lastUsed:             make(map[string]int),
		pruned:               make(map[string]bool),
	}
	p.unsubs = append(p.unsubs,
		bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(60)),
		bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithPriority(60)),
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(14)),
	)
	return p, bus
}

func TestPrunesIdleTools(t *testing.T) {
	_, bus := newPluginForTest(t)

	// Establish baseline: tool seen at turn 0.
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{
		{Name: "shell", Description: "..."},
		{Name: "web_fetch", Description: "..."},
	}}
	bus.EmitVetoable("before:llm.request", req)
	if len(req.Tools) != 2 {
		t.Fatalf("first sight should not prune, got %d tools", len(req.Tools))
	}

	// Advance turns past idle threshold for web_fetch.
	for i := 0; i < 4; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}
	// Mark shell as used at the current turn so it survives the prune.
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"})

	pruned := 0
	bus.Subscribe("memory.tool_def_pruned", func(_ engine.Event[any]) { pruned++ })

	req2 := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{
		{Name: "shell"},
		{Name: "web_fetch"},
	}}
	bus.EmitVetoable("before:llm.request", req2)
	if pruned != 1 {
		t.Fatalf("expected exactly 1 pruning event, got %d", pruned)
	}
	if len(req2.Tools) != 1 || req2.Tools[0].Name != "shell" {
		t.Fatalf("expected only shell to remain, got %v", req2.Tools)
	}
}

func TestNeverPruneList(t *testing.T) {
	_, bus := newPluginForTest(t)

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{
		{Name: "discover"},
		{Name: "anything_else"},
	}}
	bus.EmitVetoable("before:llm.request", req)

	// Advance 10 turns; discover should still be there.
	for i := 0; i < 10; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}

	req2 := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{
		{Name: "discover"},
		{Name: "anything_else"},
	}}
	bus.EmitVetoable("before:llm.request", req2)

	hasDiscover := false
	for _, td := range req2.Tools {
		if td.Name == "discover" {
			hasDiscover = true
		}
	}
	if !hasDiscover {
		t.Fatalf("discover must be exempt from pruning")
	}
}

func TestSkipsSourcedRequests(t *testing.T) {
	_, bus := newPluginForTest(t)

	bus.EmitVetoable("before:llm.request", &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{{Name: "shell"}}})
	for i := 0; i < 10; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}

	pruned := 0
	bus.Subscribe("memory.tool_def_pruned", func(_ engine.Event[any]) { pruned++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion,
		Metadata: map[string]any{"_source": "internal.plugin"},
		Tools:    []events.ToolDef{{Name: "shell"}},
	}
	bus.EmitVetoable("before:llm.request", req)
	if pruned != 0 {
		t.Fatalf("must not prune sourced requests, got %d", pruned)
	}
}

func TestRecentInvocationResetsCounter(t *testing.T) {
	_, bus := newPluginForTest(t)

	// Establish + idle.
	bus.EmitVetoable("before:llm.request", &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{{Name: "shell"}}})
	for i := 0; i < 5; i++ {
		bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion})
	}
	// Use just before the next request — should reset and survive prune.
	bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"})

	pruned := 0
	bus.Subscribe("memory.tool_def_pruned", func(_ engine.Event[any]) { pruned++ })
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Tools: []events.ToolDef{{Name: "shell"}}}
	bus.EmitVetoable("before:llm.request", req)
	if pruned != 0 {
		t.Fatalf("recently-used tool must not be pruned, got %d", pruned)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tool should remain, got %v", req.Tools)
	}
}
