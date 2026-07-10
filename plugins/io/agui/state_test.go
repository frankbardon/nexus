package agui

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
)

// newStatePlugin builds an initialized Plugin with shared-state emission
// enabled, bound to an ephemeral (unstarted) address. It never starts the HTTP
// listener — the state path is driven directly through the bus handlers.
func newStatePlugin(t *testing.T, emitState bool) *Plugin {
	t.Helper()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    engine.NewEventBus(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: map[string]any{
			"bind":       freeAddr(t),
			"emit_state": emitState,
		},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p
}

// sceneEvent builds the map[string]any payload the scene plugin emits for a
// create/patch, carrying the post-mutation content.
func sceneEvent(sceneID string, content any) engine.Event[any] {
	return engine.Event[any]{Payload: map[string]any{
		"scene_id": sceneID,
		"content":  content,
	}}
}

// collectState drains the run and returns the initial snapshot document plus the
// ordered list of state deltas. It fails if the first state event is not a
// snapshot.
func collectState(t *testing.T, r *run) (json.RawMessage, []agui.JSONPatch) {
	t.Helper()
	r.finish()
	var snapshot json.RawMessage
	var deltas []agui.JSONPatch
	for {
		select {
		case e := <-r.out:
			switch ev := e.(type) {
			case agui.StateSnapshotEvent:
				snapshot = ev.Snapshot
			case agui.StateDeltaEvent:
				deltas = append(deltas, ev.Delta)
			}
		default:
			return snapshot, deltas
		}
	}
}

// reconstruct applies deltas in order to snapshot and returns the canonical
// resulting document.
func reconstruct(t *testing.T, snapshot json.RawMessage, deltas []agui.JSONPatch) string {
	t.Helper()
	doc := snapshot
	if len(doc) == 0 {
		doc = json.RawMessage("{}")
	}
	for i, d := range deltas {
		out, err := agui.ApplyPatch(doc, d)
		if err != nil {
			t.Fatalf("apply delta %d: %v", i, err)
		}
		doc = out
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("unmarshal reconstructed: %v", err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// TestState_SnapshotAtRunStart asserts a run started with pre-existing scene
// state emits a StateSnapshot carrying that state right after RunStarted.
func TestState_SnapshotAtRunStart(t *testing.T) {
	p := newStatePlugin(t, true)

	// A scene created before any run seeds the session-scoped model. No run is
	// active, so no delta is emitted, but the model updates for the next snapshot.
	p.handleSceneCreated(sceneEvent("scene_1", map[string]any{"title": "draft"}))

	r, ok := p.startRun(runInput{threadID: "t", runID: "r"})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	snapshot, deltas := collectState(t, r)
	if len(snapshot) == 0 {
		t.Fatal("no StateSnapshot emitted at run start")
	}
	if len(deltas) != 0 {
		t.Fatalf("unexpected deltas at run start: %v", deltas)
	}
	var snap map[string]any
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	s1, ok := snap["scene_1"].(map[string]any)
	if !ok || s1["title"] != "draft" {
		t.Fatalf("snapshot = %s, want scene_1.title=draft", snapshot)
	}
}

// TestState_DeltaOnMutation asserts a scene mutation during a run emits a
// StateDelta whose ordered application to the snapshot reconstructs the final
// state.
func TestState_DeltaOnMutation(t *testing.T) {
	p := newStatePlugin(t, true)

	r, ok := p.startRun(runInput{threadID: "t", runID: "r"})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	// Mid-run scene mutations: create, patch (content replaced with merged
	// result — the scene plugin sends full post-mutation content), then delete.
	p.handleSceneCreated(sceneEvent("scene_1", map[string]any{"title": "draft", "body": "x"}))
	p.handleScenePatched(sceneEvent("scene_1", map[string]any{"title": "final", "body": "x"}))
	p.handleSceneCreated(sceneEvent("scene_2", map[string]any{"k": "v"}))
	p.handleSceneDeleted(sceneEvent("scene_1", nil))

	snapshot, deltas := collectState(t, r)
	if len(deltas) != 4 {
		t.Fatalf("delta count = %d, want 4", len(deltas))
	}

	// Applying snapshot (empty) + all deltas reconstructs the final model:
	// scene_1 deleted, scene_2 present.
	got := reconstruct(t, snapshot, deltas)
	want := `{"scene_2":{"k":"v"}}`
	if got != want {
		t.Fatalf("reconstructed = %s, want %s", got, want)
	}

	// The plugin's own model must agree with the reconstruction.
	if model := string(p.currentStateJSON()); model != want {
		t.Fatalf("plugin model = %s, want %s", model, want)
	}
}

// TestState_NoDeltaWhenContentUnchanged asserts a patch that does not change the
// content emits no delta (empty JSON Patch is suppressed).
func TestState_NoDeltaWhenContentUnchanged(t *testing.T) {
	p := newStatePlugin(t, true)
	r, ok := p.startRun(runInput{threadID: "t", runID: "r"})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	p.handleSceneCreated(sceneEvent("scene_1", map[string]any{"k": "v"}))
	p.handleScenePatched(sceneEvent("scene_1", map[string]any{"k": "v"})) // no change

	_, deltas := collectState(t, r)
	if len(deltas) != 1 {
		t.Fatalf("delta count = %d, want 1 (create only)", len(deltas))
	}
}

// TestState_DisabledEmitsNothing asserts that with emit_state off, no state
// events are produced and no scene subscriptions are declared.
func TestState_DisabledEmitsNothing(t *testing.T) {
	p := newStatePlugin(t, false)
	r, ok := p.startRun(runInput{threadID: "t", runID: "r"})
	if !ok {
		t.Fatal("startRun rejected")
	}
	t.Cleanup(func() { p.endRun(r) })

	// Even if a handler were invoked, disabled emission must produce no event.
	p.handleSceneCreated(sceneEvent("scene_1", map[string]any{"k": "v"}))

	snapshot, deltas := collectState(t, r)
	if len(snapshot) != 0 || len(deltas) != 0 {
		t.Fatalf("state events with emit_state off: snapshot=%s deltas=%v", snapshot, deltas)
	}

	// Subscriptions must not advertise scene events when disabled.
	for _, s := range p.Subscriptions() {
		for _, st := range stateEventTypes {
			if s.EventType == st {
				t.Fatalf("scene subscription %q declared with emit_state off", st)
			}
		}
	}
}
