package docssearch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin wires the tool to a bus with a stub search.lexical handler so
// the test exercises the tool.invoke -> lexical.query -> tool.result path
// without a real store.
func newTestPlugin(t *testing.T) (engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Stub lexical store: answers lexical.query with a fixed hit.
	bus.Subscribe("lexical.query", func(ev engine.Event[any]) {
		q, ok := ev.Payload.(*events.LexicalQuery)
		if !ok {
			return
		}
		q.Provider = "stub"
		q.Matches = []events.LexicalMatch{
			{ID: "h-0", Content: "BERA Score\n\n## How It Is Calculated\n\npercentile rank", Score: 2.5,
				Metadata: map[string]string{"source": "bera_score.md", "heading": "How It Is Calculated"}},
		}
	}, engine.WithPriority(10))

	p := New().(*Plugin)
	ctx := engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{"namespace": "docs", "top_k": 5},
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	return bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestSearchDocsToolResult(t *testing.T) {
	bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	var got events.ToolResult
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			got = r
		}
	}, engine.WithPriority(100))

	tc := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "search_docs",
		Arguments:     map[string]any{"query": "how is bera score calculated"},
		TurnID:        "turn-1",
	}
	if err := bus.Emit("tool.invoke", tc); err != nil {
		t.Fatalf("emit tool.invoke: %v", err)
	}

	if got.ID != "call-1" {
		t.Fatalf("no tool.result captured (got ID %q)", got.ID)
	}
	if got.Error != "" {
		t.Fatalf("tool.result error: %s", got.Error)
	}

	var out struct {
		Query   string `json:"query"`
		Count   int    `json:"count"`
		Results []struct {
			Source  string `json:"source"`
			Heading string `json:"heading"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, got.Output)
	}
	if out.Count != 1 || len(out.Results) != 1 {
		t.Fatalf("want 1 result, got %d", out.Count)
	}
	if out.Results[0].Source != "bera_score.md" || out.Results[0].Heading != "How It Is Calculated" {
		t.Errorf("result = %+v, want bera_score.md / How It Is Calculated", out.Results[0])
	}
}

func TestSearchDocsEmptyQuery(t *testing.T) {
	bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	var got events.ToolResult
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			got = r
		}
	}, engine.WithPriority(100))

	tc := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "call-2", Name: "search_docs",
		Arguments: map[string]any{"query": "   "}}
	if err := bus.Emit("tool.invoke", tc); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Error == "" {
		t.Errorf("expected error for empty query, got output %q", got.Output)
	}
}
