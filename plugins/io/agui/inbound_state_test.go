package agui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	scenepkg "github.com/frankbardon/nexus/plugins/scene"
)

// newStatePluginOnBus builds a state-enabled AG-UI plugin sharing the given bus,
// so inbound scene seeds can drive a real scene plugin on the same bus.
func newStatePluginOnBus(t *testing.T, bus engine.EventBus) *Plugin {
	t.Helper()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: map[string]any{
			"bind":       freeAddr(t),
			"emit_state": true,
		},
	}); err != nil {
		t.Fatalf("init agui: %v", err)
	}
	return p
}

// newScenePluginOnBus wires a real nexus.scene plugin onto the bus with an
// in-memory (no data dir) store so the AG-UI inbound seeds land in a store the
// agent could read via scene_get.
func newScenePluginOnBus(t *testing.T, bus engine.EventBus, sessionID string) *scenepkg.Plugin {
	t.Helper()
	sp := scenepkg.New().(*scenepkg.Plugin)
	if err := sp.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: &engine.SessionWorkspace{ID: sessionID},
	}); err != nil {
		t.Fatalf("init scene: %v", err)
	}
	if err := sp.Ready(); err != nil {
		t.Fatalf("ready scene: %v", err)
	}
	t.Cleanup(func() { _ = sp.Shutdown(context.Background()) })
	return sp
}

// TestInboundState_SeedsSnapshotAndScene asserts a RunAgentInput.state seeds both
// the emitted StateSnapshot (client's view before the agent runs) and the scene
// store (so the agent observes it via scene_get).
func TestInboundState_SeedsSnapshotAndScene(t *testing.T) {
	bus := engine.NewEventBus()
	sp := newScenePluginOnBus(t, bus, "thread-1")
	p := newStatePluginOnBus(t, bus)

	state := json.RawMessage(`{"scene_1":{"title":"from-client"}}`)
	r, ok := p.startRun(runInput{threadID: "thread-1", runID: "r1", state: state})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	// The snapshot emitted at run start must carry the client's seeded scene.
	snapshot, deltas := collectState(t, r)
	if len(snapshot) == 0 {
		t.Fatal("no StateSnapshot emitted")
	}
	var snap map[string]any
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	s1, ok := snap["scene_1"].(map[string]any)
	if !ok || s1["title"] != "from-client" {
		t.Fatalf("snapshot = %s, want scene_1.title=from-client", snapshot)
	}
	// Seeding the mirror to the same value the scene store echoes back must not
	// produce a spurious delta.
	if len(deltas) != 0 {
		t.Fatalf("unexpected deltas from seed: %v", deltas)
	}

	// The scene store — what the agent reads — must now hold the seeded scene.
	sc, err := sp.Store().Get("thread-1", "scene_1")
	if err != nil {
		t.Fatalf("scene_get after seed: %v", err)
	}
	content, ok := sc.Content.(map[string]any)
	if !ok || content["title"] != "from-client" {
		t.Fatalf("scene content = %#v, want title=from-client", sc.Content)
	}
}

// TestInboundState_ClientPatchMergesExistingScene asserts a client state entry
// for a scene the agent already created merges (last-writer per key) rather than
// replacing the whole scene, and the merged result is visible to the agent.
func TestInboundState_ClientPatchMergesExistingScene(t *testing.T) {
	bus := engine.NewEventBus()
	sp := newScenePluginOnBus(t, bus, "thread-1")
	p := newStatePluginOnBus(t, bus)

	// Agent-created scene under a known id via the scene tool.
	if err := bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		Name:          "scene_create",
		Arguments: map[string]any{
			"scene_id": "board",
			"content":  map[string]any{"title": "agent", "body": "keep"},
		},
	}); err != nil {
		t.Fatalf("agent create: %v", err)
	}

	// Client edits only the title; body must be preserved by the shallow merge.
	state := json.RawMessage(`{"board":{"title":"client-edit"}}`)
	r, ok := p.startRun(runInput{threadID: "thread-1", runID: "r1", state: state})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })
	_, _ = collectState(t, r)

	sc, err := sp.Store().Get("thread-1", "board")
	if err != nil {
		t.Fatalf("scene_get: %v", err)
	}
	content, _ := sc.Content.(map[string]any)
	if content["title"] != "client-edit" {
		t.Fatalf("title = %v, want client-edit", content["title"])
	}
	if content["body"] != "keep" {
		t.Fatalf("body = %v, want keep (client patch must merge, not replace)", content["body"])
	}
}

// TestInboundState_AgentWinsWithinRun asserts the documented conflict semantics:
// the client seed is applied before the run, then an agent-side scene mutation
// during the run is last-writer and flows back out as a StateDelta (round-trip).
func TestInboundState_AgentWinsWithinRun(t *testing.T) {
	bus := engine.NewEventBus()
	sp := newScenePluginOnBus(t, bus, "thread-1")
	p := newStatePluginOnBus(t, bus)

	state := json.RawMessage(`{"doc":{"status":"client"}}`)
	r, ok := p.startRun(runInput{threadID: "thread-1", runID: "r1", state: state})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	// Agent mutates the same scene mid-run; last-writer wins.
	if err := bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		Name:          "scene_patch",
		Arguments: map[string]any{
			"scene_id": "doc",
			"patch":    map[string]any{"status": "agent"},
		},
	}); err != nil {
		t.Fatalf("agent patch: %v", err)
	}

	snapshot, deltas := collectState(t, r)

	// Snapshot reflects the client seed; the agent's mutation rides a delta.
	var snap map[string]any
	_ = json.Unmarshal(snapshot, &snap)
	if d, _ := snap["doc"].(map[string]any); d["status"] != "client" {
		t.Fatalf("snapshot doc.status = %v, want client (seed)", d)
	}
	if len(deltas) == 0 {
		t.Fatal("expected a StateDelta for the agent mutation")
	}
	got := reconstruct(t, snapshot, deltas)
	if got != `{"doc":{"status":"agent"}}` {
		t.Fatalf("reconstructed = %s, want agent wins", got)
	}
	// Store agrees: agent wins.
	sc, _ := sp.Store().Get("thread-1", "doc")
	content, _ := sc.Content.(map[string]any)
	if content["status"] != "agent" {
		t.Fatalf("store doc.status = %v, want agent", content["status"])
	}
}

// TestInboundState_DisabledIgnored asserts inbound state is a no-op when
// shared-state emission is off.
func TestInboundState_DisabledIgnored(t *testing.T) {
	bus := engine.NewEventBus()
	sp := newScenePluginOnBus(t, bus, "thread-1")

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: map[string]any{"bind": freeAddr(t), "emit_state": false},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	state := json.RawMessage(`{"scene_x":{"k":"v"}}`)
	r, ok := p.startRun(runInput{threadID: "thread-1", runID: "r1", state: state})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })
	_, _ = collectState(t, r)

	if _, err := sp.Store().Get("thread-1", "scene_x"); err == nil {
		t.Fatal("scene seeded with emit_state off")
	}
}

// TestInboundState_MalformedIgnored asserts a non-object state document is
// skipped without failing the run and without touching the mirror.
func TestInboundState_MalformedIgnored(t *testing.T) {
	p := newStatePlugin(t, true)
	// A JSON array is not a scene-keyed object.
	p.applyInboundState("thread-1", json.RawMessage(`[1,2,3]`))
	if got := string(p.currentStateJSON()); got != "{}" {
		t.Fatalf("mirror = %s, want empty after malformed inbound state", got)
	}
}
