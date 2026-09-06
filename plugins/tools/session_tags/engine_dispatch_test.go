package session_tags

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file is E4-S3's addition: contract_test.go's ContractHarness builds
// its own bus and SessionWorkspace by hand and calls Plugin.Init/Ready
// directly — it never calls engine.Engine.Boot, so
// installSessionTagHandlers (pkg/engine/session_tags.go, the sole
// enforcement point that rejects a reserved key on the general
// before:session.tag.set/delete path) is never wired in that harness. Every
// reserved-namespace assertion there depends on the test itself standing in
// for that handler (see TestSessionTagSet_NoLocalBypass and
// TestSessionTagSet_RespectsVeto).
//
// This file closes that gap: it boots a REAL engine.Engine — the public
// New + Boot entry point, exactly as cmd/nexus does — with this package's
// actual Plugin registered and activated the same way an operator's config
// would, then drives a simulated LLM tool call through the same tool.invoke
// bus event a real agent loop's tool dispatch uses. That means
// before:session.tag.set/delete is protected by the real
// installSessionTagHandlers, not a test-supplied stand-in, so a passing
// test here proves the reserved-namespace lockout holds through this
// plugin's actual tool-call surface end to end.

// newBootedEngine boots a full, real engine.Engine with this package's
// Plugin as the only active plugin. Session and storage roots point at a
// per-test temp dir so nothing touches the real ~/.nexus tree.
func newBootedEngine(t *testing.T) *engine.Engine {
	t.Helper()
	root := t.TempDir()

	eng, err := engine.New("") // configPath == "" -> engine.DefaultConfig()
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	eng.Config.Core.Sessions.Root = filepath.Join(root, "sessions")
	eng.Config.Core.Storage.Root = root
	eng.Config.Plugins.Active = []string{pluginID}

	eng.Registry.Register(pluginID, New)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// toolResultCollector subscribes to tool.result on a real engine's bus and
// hands back a lookup-by-call-ID accessor. Bus dispatch is synchronous (see
// CLAUDE.md's "Dispatched synchronously"), so every result is present by the
// time the Emit that triggered it returns.
func toolResultCollector(eng *engine.Engine) func(callID string) (events.ToolResult, bool) {
	results := map[string]events.ToolResult{}
	eng.Bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if tr, ok := e.Payload.(events.ToolResult); ok {
			results[tr.ID] = tr
		}
	})
	return func(callID string) (events.ToolResult, bool) {
		tr, ok := results[callID]
		return tr, ok
	}
}

// invokeTool emits a tool.invoke exactly as a real agent's tool dispatch
// would after an LLM response requested a tool call.
func invokeTool(t *testing.T, eng *engine.Engine, callID, name string, args map[string]any) {
	t.Helper()
	if err := eng.Bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            callID,
		Name:          name,
		Arguments:     args,
	}); err != nil {
		t.Fatalf("Emit tool.invoke %q: %v", name, err)
	}
}

// TestRealEngine_SessionTagSet_ReservedKeyRejected drives a simulated LLM
// tool call to session_tag_set with a reserved-prefixed key through the real
// tool-dispatch path (tool.invoke -> this plugin's handleSet ->
// before:session.tag.set -> the real installSessionTagHandlers) and proves
// it is rejected end to end: a tool error, no session.tag.set announcement,
// and Labels never actually written.
func TestRealEngine_SessionTagSet_ReservedKeyRejected(t *testing.T) {
	eng := newBootedEngine(t)
	getResult := toolResultCollector(eng)

	var announced int
	eng.Bus.Subscribe("session.tag.set", func(engine.Event[any]) { announced++ })

	invokeTool(t, eng, "call-set-reserved", "session_tag_set",
		map[string]any{"key": "_principal_id", "value": "attacker-controlled"})

	tr, ok := getResult("call-set-reserved")
	if !ok {
		t.Fatal("no tool.result observed for session_tag_set")
	}
	if tr.Error == "" {
		t.Fatal("expected a reserved-key session_tag_set to be rejected end-to-end through the real engine, got no error")
	}
	if tr.OutputStructured != nil {
		t.Fatalf("expected no structured output on a rejected set, got %+v", tr.OutputStructured)
	}
	if announced != 0 {
		t.Fatalf("expected no session.tag.set announcement for a rejected write, got %d", announced)
	}

	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, present := meta.Labels["_principal_id"]; present {
		t.Fatal("reserved key was written to Labels via the real tool-dispatch path despite rejection")
	}
}

// TestRealEngine_SessionTagDelete_ReservedKeyRejected mirrors the set case
// for delete: a pre-existing reserved label (seeded the only sanctioned way,
// via SetReservedLabel, standing in for nexus.io.agui's identity binding)
// must survive a session_tag_delete tool call targeting it.
func TestRealEngine_SessionTagDelete_ReservedKeyRejected(t *testing.T) {
	eng := newBootedEngine(t)
	if err := eng.Session.SetReservedLabel("_principal_id", "user-1"); err != nil {
		t.Fatalf("seed SetReservedLabel: %v", err)
	}
	getResult := toolResultCollector(eng)

	invokeTool(t, eng, "call-delete-reserved", "session_tag_delete", map[string]any{"key": "_principal_id"})

	tr, ok := getResult("call-delete-reserved")
	if !ok {
		t.Fatal("no tool.result observed for session_tag_delete")
	}
	if tr.Error == "" {
		t.Fatal("expected a reserved-key session_tag_delete to be rejected end-to-end through the real engine, got no error")
	}

	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels["_principal_id"] != "user-1" {
		t.Fatalf("Labels[_principal_id] = %q, want unchanged %q after a rejected delete", meta.Labels["_principal_id"], "user-1")
	}
}

// TestRealEngine_SessionTagGet_ReservedKeyNeverReturned confirms a reserved
// key already present in Labels — set directly, the only sanctioned write
// path — is still reported not-found through a real session_tag_get tool
// call, with no value field leaking either way.
func TestRealEngine_SessionTagGet_ReservedKeyNeverReturned(t *testing.T) {
	eng := newBootedEngine(t)
	if err := eng.Session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("seed SetReservedLabel: %v", err)
	}
	getResult := toolResultCollector(eng)

	invokeTool(t, eng, "call-get-reserved", "session_tag_get", map[string]any{"key": "_principal_id"})

	tr, ok := getResult("call-get-reserved")
	if !ok {
		t.Fatal("no tool.result observed for session_tag_get")
	}
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	if found, _ := tr.OutputStructured["found"].(bool); found {
		t.Fatal("expected found=false for a reserved-prefixed key even though it is set, via the real tool-dispatch path")
	}
	if _, present := tr.OutputStructured["value"]; present {
		t.Fatal("expected no value field to leak for a reserved-prefixed key via the real tool-dispatch path")
	}
}

// TestRealEngine_SessionTagList_FiltersReservedKeyAlongsideGeneralOnes seeds
// both a reserved label (direct write) and a general one (through the real
// tool-dispatch path itself, so the test proves filtering rather than an
// empty store) and confirms session_tag_list returns only the general one.
func TestRealEngine_SessionTagList_FiltersReservedKeyAlongsideGeneralOnes(t *testing.T) {
	eng := newBootedEngine(t)
	if err := eng.Session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("seed SetReservedLabel: %v", err)
	}
	getResult := toolResultCollector(eng)

	invokeTool(t, eng, "call-set-general", "session_tag_set", map[string]any{"key": "project", "value": "widgets"})
	if tr, ok := getResult("call-set-general"); !ok || tr.Error != "" {
		t.Fatalf("setup: general session_tag_set failed via the real tool-dispatch path: ok=%v tr=%+v", ok, tr)
	}

	invokeTool(t, eng, "call-list", "session_tag_list", map[string]any{})
	tr, ok := getResult("call-list")
	if !ok {
		t.Fatal("no tool.result observed for session_tag_list")
	}
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	tags, ok := tr.OutputStructured["tags"].(map[string]string)
	if !ok {
		t.Fatalf("tags field missing or wrong type: %#v", tr.OutputStructured["tags"])
	}
	if _, present := tags["_principal_id"]; present {
		t.Fatal("reserved key leaked into session_tag_list result via the real tool-dispatch path")
	}
	if tags["project"] != "widgets" {
		t.Fatalf("tags[project] = %q, want widgets", tags["project"])
	}
	if len(tags) != 1 {
		t.Fatalf("tags = %+v, want exactly 1 general-namespace entry", tags)
	}
}
