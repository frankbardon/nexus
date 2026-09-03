package scene

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This plugin already emitted eight event types, none of them about the two
// files it writes, so it read as instrumented while being entirely invisible
// to anything syncing the session tree. These tests pin the fix: the patch
// journal — the durable source of truth the replay primitive reconstructs
// scene state from — announces every append as it lands, and scenes.json
// announces its snapshot.
func TestPlugin_PatchJournalAppendIsAnnounced(t *testing.T) {
	bus := engine.NewEventBus()
	ws, err := engine.NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	record := func(ev engine.Event[any]) {
		data, _ := ev.Payload.(map[string]any)
		path, _ := data["path"].(string)
		added, _ := data["bytes_added"].(int)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf("%s %s added>0=%t", ev.Type, path, added > 0))
	}
	bus.Subscribe("session.file.created", record)
	bus.Subscribe("session.file.updated", record)

	p := New().(*Plugin)
	// DataDir is the session-scoped plugin dir the engine hands every plugin;
	// the announcement is worthless if it does not resolve to a path under the
	// session root, so the test uses the real accessor rather than a TempDir.
	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: ws,
		DataDir: ws.PluginDir(pluginID),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}

	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "c1",
		Name:          "scene_create",
		Arguments:     map[string]any{"schema": "chart", "content": map[string]any{"a": 1}},
	})

	mu.Lock()
	afterCreate := slices.Clone(seen)
	mu.Unlock()

	want := "session.file.created plugins/nexus.scene/scenes.jsonl added>0=true"
	if !slices.Contains(afterCreate, want) {
		t.Fatalf("emissions after scene_create = %v, want to contain %q", afterCreate, want)
	}

	// Shutdown persists scenes.json. Announcing from Shutdown is safe — a bus
	// Emit after the journal writer closes is dropped rather than stalling it —
	// and a resumed session reads this file, so it has to be visible.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	mu.Lock()
	final := slices.Clone(seen)
	mu.Unlock()

	wantState := "session.file.created plugins/nexus.scene/scenes.json added>0=true"
	if !slices.Contains(final, wantState) {
		t.Fatalf("emissions after shutdown = %v, want to contain %q", final, wantState)
	}
}

// Emissions() is the contract harness's only view of what a plugin does, so a
// declaration that omits the two new types would let a future refactor delete
// them without a test noticing.
func TestPlugin_DeclaresSessionFileEmissions(t *testing.T) {
	declared := New().Emissions()
	for _, want := range []string{"session.file.created", "session.file.updated"} {
		if !slices.Contains(declared, want) {
			t.Errorf("Emissions() = %v, missing %q", declared, want)
		}
	}
}

// Plugin unit tests routinely build a bare &SessionWorkspace{ID: "..."} with
// no bus and no root. Announcing through one must stay inert.
func TestPlugin_BareSessionWorkspaceDoesNotPanic(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: &engine.SessionWorkspace{ID: "sess-1"},
		DataDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	_ = bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "c1",
		Name:          "scene_create",
		Arguments:     map[string]any{"schema": "chart", "content": map[string]any{"a": 1}},
	})
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
