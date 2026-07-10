package scene

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func TestPlugin_CreateAndPatchViaTools(t *testing.T) {
	dir := t.TempDir()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  logger,
		Session: &engine.SessionWorkspace{ID: "sess-1"},
		DataDir: dir,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// Capture tool results.
	results := make(chan events.ToolResult, 4)
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			results <- r
		}
	})

	// Capture scene.created / scene.patched to assert they carry post-mutation
	// content (the AG-UI shared-state mirror relies on this).
	created := make(chan map[string]any, 1)
	patched := make(chan map[string]any, 1)
	bus.Subscribe("scene.created", func(ev engine.Event[any]) {
		if m, ok := ev.Payload.(map[string]any); ok {
			created <- m
		}
	})
	bus.Subscribe("scene.patched", func(ev engine.Event[any]) {
		if m, ok := ev.Payload.(map[string]any); ok {
			patched <- m
		}
	})

	// scene_create
	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "c1",
		Name:          "scene_create",
		Arguments: map[string]any{
			"schema":  "doc.markdown",
			"content": map[string]any{"text": "hello"},
		},
		TurnID: "t1",
	})
	r1 := <-results
	if r1.Error != "" {
		t.Fatalf("create error: %s", r1.Error)
	}
	// scene.created must carry the initial content.
	c1 := <-created
	if content, ok := c1["content"].(map[string]any); !ok || content["text"] != "hello" {
		t.Fatalf("scene.created content = %v, want text=hello", c1["content"])
	}
	var handle1 map[string]any
	_ = json.Unmarshal([]byte(r1.Output), &handle1)
	sceneID, _ := handle1["id"].(string)
	if sceneID == "" {
		t.Fatalf("no scene id")
	}

	// scene_patch
	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "c2",
		Name:          "scene_patch",
		Arguments: map[string]any{
			"scene_id": sceneID,
			"patch":    map[string]any{"text": "world"},
		},
		TurnID: "t1",
	})
	r2 := <-results
	if r2.Error != "" {
		t.Fatalf("patch error: %s", r2.Error)
	}
	// scene.patched must carry the full post-patch content (shallow-merged).
	p2 := <-patched
	if content, ok := p2["content"].(map[string]any); !ok || content["text"] != "world" {
		t.Fatalf("scene.patched content = %v, want text=world", p2["content"])
	}

	// scene_get
	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "c3",
		Name:          "scene_get",
		Arguments:     map[string]any{"scene_id": sceneID},
		TurnID:        "t1",
	})
	r3 := <-results
	if r3.Error != "" {
		t.Fatalf("get error: %s", r3.Error)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(r3.Output), &got)
	content := got["content"].(map[string]any)
	if content["text"] != "world" {
		t.Errorf("post-patch text = %v", content["text"])
	}
}
