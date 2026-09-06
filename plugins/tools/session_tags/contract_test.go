package session_tags

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract asserts the plugin's declared Subscriptions()/Emissions()
// match runtime behavior — the baseline hygiene every plugin's contract test
// carries (see docs/src/guides/plugin-contracts.md). Deeper adversarial
// coverage of the reserved-namespace lockout exercised specifically through
// this plugin's tool-call surface is E4-S3's job, not this one's.
func TestContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	h.AssertSubscribesTo("tool.invoke")
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"before:tool.result", "tool.result", "tool.register",
		"before:session.tag.set", "before:session.tag.delete",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}

// seedLabels writes meta.Labels directly to the harness's session metadata
// file, bypassing the bus entirely — these tests only need Labels to already
// contain certain keys before the plugin's tool.invoke handler reads them.
func seedLabels(t *testing.T, sessionDir string, labels map[string]string) {
	t.Helper()
	path := filepath.Join(sessionDir, "metadata", "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	var meta engine.SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal session.json: %v", err)
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	maps.Copy(meta.Labels, labels)
	out, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal session.json: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
}

// invokeAndCapture injects a tool.invoke for the given tool/args and returns
// the resulting events.ToolResult.
func invokeAndCapture(t *testing.T, h *contract.ContractHarness, name string, args map[string]any) events.ToolResult {
	t.Helper()
	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          name,
		Arguments:     args,
	})
	for _, e := range h.PluginEmissions() {
		if e.Type != "tool.result" {
			continue
		}
		if tr, ok := e.Payload.(events.ToolResult); ok {
			return tr
		}
	}
	t.Fatalf("no tool.result observed for %q", name)
	return events.ToolResult{}
}

func TestSessionTagGet_ReservedKeyNotFound(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	seedLabels(t, h.SessionDir(), map[string]string{"_principal_id": "user-42"})

	tr := invokeAndCapture(t, h, "session_tag_get", map[string]any{"key": "_principal_id"})
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	found, _ := tr.OutputStructured["found"].(bool)
	if found {
		t.Fatal("expected found=false for a reserved-prefixed key, even though it is set")
	}
	if _, ok := tr.OutputStructured["value"]; ok {
		t.Fatal("expected no value field to leak for a reserved-prefixed key")
	}
}

func TestSessionTagGet_GeneralKeyFound(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	seedLabels(t, h.SessionDir(), map[string]string{"project": "widgets"})

	tr := invokeAndCapture(t, h, "session_tag_get", map[string]any{"key": "project"})
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	found, _ := tr.OutputStructured["found"].(bool)
	if !found {
		t.Fatal("expected found=true for a set general-namespace key")
	}
	if v, _ := tr.OutputStructured["value"].(string); v != "widgets" {
		t.Fatalf("value = %q, want widgets", v)
	}
}

func TestSessionTagGet_MissingKeyNotFound(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())

	tr := invokeAndCapture(t, h, "session_tag_get", map[string]any{"key": "nope"})
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	if found, _ := tr.OutputStructured["found"].(bool); found {
		t.Fatal("expected found=false for a key that was never set")
	}
}

func TestSessionTagList_FiltersReservedKeys(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())
	seedLabels(t, h.SessionDir(), map[string]string{
		"_principal_id": "user-42",
		"project":       "widgets",
		"tenant":        "acme",
	})

	tr := invokeAndCapture(t, h, "session_tag_list", map[string]any{})
	if tr.Error != "" {
		t.Fatalf("unexpected error: %q", tr.Error)
	}
	tags, ok := tr.OutputStructured["tags"].(map[string]string)
	if !ok {
		t.Fatalf("tags field missing or wrong type: %#v", tr.OutputStructured["tags"])
	}
	if _, present := tags["_principal_id"]; present {
		t.Fatal("reserved key leaked into session_tag_list result")
	}
	if tags["project"] != "widgets" || tags["tenant"] != "acme" {
		t.Fatalf("tags = %+v, want project=widgets tenant=acme", tags)
	}
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want exactly 2 general-namespace entries", tags)
	}
}

// TestSessionTagSet_NoLocalBypass proves the plugin does no reserved-key
// enforcement of its own: with nothing subscribed on before:session.tag.set
// (the sole enforcement point, owned by the core engine, is not wired into
// this isolated harness), the plugin defers entirely to the bus outcome. A
// version of this plugin that duplicated the reserved-prefix check locally
// would instead reject the call even here, with no veto in play.
func TestSessionTagSet_NoLocalBypass(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())

	var captured *events.SessionTagSetRequest
	h.Bus().Subscribe("before:session.tag.set", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		captured, _ = vp.Original.(*events.SessionTagSetRequest)
		// Deliberately does not veto — simulates "no enforcement handler
		// present", isolating this plugin's own behavior from the core
		// engine's installSessionTagHandlers.
	})

	tr := invokeAndCapture(t, h, "session_tag_set", map[string]any{"key": "_principal_id", "value": "spoofed"})
	if tr.Error != "" {
		t.Fatalf("plugin rejected a reserved key locally (no bypass expected): %q", tr.Error)
	}
	if captured == nil || captured.Key != "_principal_id" || captured.Value != "spoofed" {
		t.Fatalf("before:session.tag.set request payload = %+v, want Key=_principal_id Value=spoofed", captured)
	}
}

// TestSessionTagSet_RespectsVeto proves the plugin surfaces a veto verbatim
// as a tool error rather than swallowing or reinterpreting it — the same
// outcome a reserved-key attempt gets from the real core handler.
func TestSessionTagSet_RespectsVeto(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())

	const reason = "session tag key is reserved"
	h.Bus().Subscribe("before:session.tag.set", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: reason}
	})

	tr := invokeAndCapture(t, h, "session_tag_set", map[string]any{"key": "_principal_id", "value": "spoofed"})
	if tr.Error != reason {
		t.Fatalf("tool result Error = %q, want %q", tr.Error, reason)
	}
	if tr.OutputStructured != nil {
		t.Fatalf("expected no structured output on a vetoed set, got %+v", tr.OutputStructured)
	}
}

// TestSessionTagDelete_RespectsVeto mirrors TestSessionTagSet_RespectsVeto
// for the delete path.
func TestSessionTagDelete_RespectsVeto(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession())

	const reason = "session tag key is reserved"
	h.Bus().Subscribe("before:session.tag.delete", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: reason}
	})

	tr := invokeAndCapture(t, h, "session_tag_delete", map[string]any{"key": "_principal_id"})
	if tr.Error != reason {
		t.Fatalf("tool result Error = %q, want %q", tr.Error, reason)
	}
}

func TestToolsDisabledByConfig(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"tools": map[string]any{
			"session_tag_set": false,
		},
	}))

	registered := map[string]bool{}
	for _, e := range h.PluginEmissions() {
		if e.Type != "tool.register" {
			continue
		}
		if def, ok := e.Payload.(events.ToolDef); ok {
			registered[def.Name] = true
		}
	}
	if registered["session_tag_set"] {
		t.Fatal("session_tag_set should not be registered when disabled by config")
	}
	if !registered["session_tag_get"] || !registered["session_tag_delete"] || !registered["session_tag_list"] {
		t.Fatalf("expected other three tools still registered, got %+v", registered)
	}
}
