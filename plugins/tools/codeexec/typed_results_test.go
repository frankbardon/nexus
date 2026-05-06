package codeexec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// registerTypedTool registers a fake "search" tool that declares an output
// schema and populates OutputStructured. Returns the handler's call counter
// so tests can verify invocations.
func (h *testHarness) registerTypedTool() *int {
	calls := 0
	_ = h.bus.Emit("tool.register", events.ToolDef{
		Name:        "search",
		Description: "Search something",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []any{"query"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"hits": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":    map[string]any{"type": "string"},
							"score": map[string]any{"type": "number"},
						},
						"required": []any{"id", "score"},
					},
				},
				"total": map[string]any{"type": "integer"},
			},
			"required": []any{"hits", "total"},
		},
	})

	h.bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, ok := e.Payload.(events.ToolCall)
		if !ok || tc.Name != "search" {
			return
		}
		calls++
		query, _ := tc.Arguments["query"].(string)
		_ = h.bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
			Name:   tc.Name,
			Output: "search result for " + query,
			OutputStructured: map[string]any{
				"hits": []any{
					map[string]any{"id": "a", "score": 0.9},
					map[string]any{"id": "b", "score": 0.5},
				},
				"total": 2,
			},
			TurnID: tc.TurnID,
		})
	}, engine.WithPriority(40), engine.WithSource("fake-typed"))

	return &calls
}

func TestTypedResult_ScriptReadsTypedFields(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.registerTypedTool()

	script := `package main

import (
	"context"
	"fmt"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	r, err := tools.Search(tools.SearchArgs{Query: "hello", Limit: 5})
	if err != nil {
		return nil, err
	}
	fmt.Println("total:", r.Total)
	fmt.Println("first id:", r.Hits[0].Id)
	fmt.Printf("first score: %.2f\n", r.Hits[0].Score)
	return map[string]any{
		"total":       r.Total,
		"first_id":    r.Hits[0].Id,
		"first_score": r.Hits[0].Score,
	}, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}

	var env map[string]any
	if err := json.Unmarshal([]byte(res.Output), &env); err != nil {
		t.Fatalf("bad envelope: %v (%q)", err, res.Output)
	}

	stdout, _ := env["stdout"].(string)
	for _, want := range []string{"total: 2", "first id: a", "first score: 0.90"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q, got: %q", want, stdout)
		}
	}

	result, _ := env["result"].(map[string]any)
	if int(result["total"].(float64)) != 2 {
		t.Errorf("want total=2, got %v", result["total"])
	}
	if result["first_id"] != "a" {
		t.Errorf("want first_id=a, got %v", result["first_id"])
	}
}

// If a tool declares an OutputSchema but only populates Output (not
// OutputStructured), the shim should still decode the JSON payload into the
// typed struct so older tools migrating over work seamlessly.
func TestTypedResult_FallsBackToOutputJSON(t *testing.T) {
	h := newHarness(t, nil)

	_ = h.bus.Emit("tool.register", events.ToolDef{
		Name: "stats",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{"type": "integer"},
				"label": map[string]any{"type": "string"},
			},
			"required": []any{"count", "label"},
		},
	})
	h.bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, ok := e.Payload.(events.ToolCall)
		if !ok || tc.Name != "stats" {
			return
		}
		// Deliberately only populate Output with legacy JSON, not OutputStructured.
		_ = h.bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
			Name:   tc.Name,
			Output: `{"count":42,"label":"legacy"}`,
			TurnID: tc.TurnID,
		})
	}, engine.WithPriority(40), engine.WithSource("fake-legacy"))

	script := `package main

import (
	"context"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	r, err := tools.Stats(tools.StatsArgs{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"count": r.Count, "label": r.Label}, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	result, _ := env["result"].(map[string]any)
	if int(result["count"].(float64)) != 42 {
		t.Errorf("want count=42, got %v", result["count"])
	}
	if result["label"] != "legacy" {
		t.Errorf("want label=legacy, got %v", result["label"])
	}
}

// Schema-less tools keep the old tools.Result shape — scripts that mixed
// both shapes should still compile.
func TestTypedResult_SchemalessStillUsesResult(t *testing.T) {
	h := newHarness(t, nil)
	h.registerFakeTool()      // legacy "echo" has no OutputSchema
	_ = h.registerTypedTool() // typed "search"

	script := `package main

import (
	"context"
	"tools"
)

func Run(ctx context.Context) (any, error) {
	// Schema-less: still returns tools.Result.
	er, _ := tools.Echo(tools.EchoArgs{Message: "hi"})
	// Typed: returns tools.SearchResult.
	sr, _ := tools.Search(tools.SearchArgs{Query: "q"})
	return map[string]any{
		"echo":  er.Output,
		"total": sr.Total,
	}, nil
}
`
	res := h.runCode(script)
	if res.Error != "" {
		t.Fatalf("err: %s", res.Error)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(res.Output), &env)
	result, _ := env["result"].(map[string]any)
	if result["echo"] != "echoed: hi" {
		t.Errorf("want echoed: hi, got %v", result["echo"])
	}
	if int(result["total"].(float64)) != 2 {
		t.Errorf("want total=2, got %v", result["total"])
	}
}
