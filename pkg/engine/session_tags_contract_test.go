package engine

import (
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// This file is E1-S2's additional, more adversarial coverage for the
// reserved-namespace invariant already exercised (from a direct-call angle)
// by session_tags_test.go under E1-S1. It does not duplicate those tests;
// it drives the same enforcement point from realistic multi-subscriber
// angles: a tool-call-triggered emission standing in for the future
// nexus.tool.session_tags plugin, a side-by-side comparison of the general
// and reserved write paths' announce payloads, a real session-resume
// round-trip, and a "declared emissions" style conformance check for the
// bus surface installSessionTagHandlers owns.
//
// There is no engine.Plugin here for pkg/testharness/contract.NewContract to
// wrap — installSessionTagHandlers is core engine wiring (called from Boot),
// not a plugin with its own Subscriptions()/Emissions(). So this file
// reproduces the harness's real-engine.Bus, capture-and-assert shape by hand
// instead of importing the contract package, which needs a
// engine.PluginFactory this component doesn't have. See FOLLOWUPS in the
// story handoff for the same note.

// TestBeforeSessionTagSet_ReservedKeyRejected_ViaToolCallPath is the
// adversarial-angle test the story asks for: a reserved key must never reach
// Labels even when the before:session.tag.set request originates from a
// plausible tool-call path rather than a bare EmitVetoable call in a test
// body.
//
// It stands in for the not-yet-built nexus.tool.session_tags plugin: a
// handler subscribed to tool.invoke (the same event every real tool
// dispatch rides — see pkg/engine/toolcache.go) parses an LLM-supplied
// ToolCall.Arguments map and forwards it as a SessionTagSetRequest, exactly
// as that plugin will. Driving the attack through tool.invoke — the
// surface an untrusted or prompt-injected LLM tool call actually uses —
// proves the enforcement point in installSessionTagHandlers holds
// regardless of which caller reaches it, not just the one call shape a
// direct unit test happens to use.
func TestBeforeSessionTagSet_ReservedKeyRejected_ViaToolCallPath(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	var toolVeto VetoResult
	var toolVetoErr error
	bus.Subscribe("tool.invoke", func(ev Event[any]) {
		tc, ok := ev.Payload.(events.ToolCall)
		if !ok || tc.Name != "session_tag_set" {
			return
		}
		key, _ := tc.Arguments["key"].(string)
		value, _ := tc.Arguments["value"].(string)
		req := &events.SessionTagSetRequest{
			SchemaVersion: events.SessionTagSetRequestVersion,
			Key:           key,
			Value:         value,
		}
		toolVeto, toolVetoErr = bus.EmitVetoable("before:session.tag.set", req)
	})

	var announced int
	bus.Subscribe("session.tag.set", func(Event[any]) { announced++ })

	// An LLM tricked (e.g. via prompt injection in tool output it read
	// earlier) into invoking the session-tag tool with a reserved key,
	// attempting to overwrite the identity binding rather than set an
	// ordinary user tag.
	if err := bus.Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-attacker-1",
		Name:          "session_tag_set",
		Arguments:     map[string]any{"key": "_principal_id", "value": "attacker-controlled"},
	}); err != nil {
		t.Fatalf("Emit tool.invoke: %v", err)
	}

	if toolVetoErr != nil {
		t.Fatalf("EmitVetoable via tool-call path: %v", toolVetoErr)
	}
	if !toolVeto.Vetoed {
		t.Fatal("expected reserved key requested via a tool-call path to be vetoed")
	}
	if toolVeto.Reason == "" {
		t.Error("expected a non-empty veto reason")
	}
	if announced != 0 {
		t.Fatalf("expected no session.tag.set announcement for the attacker's write, got %d", announced)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; ok {
		t.Fatal("reserved key was written to Labels via the tool-call path despite veto")
	}
}

// TestSessionTagAnnounce_GeneralAndReservedPathsMatch confirms both routes
// into applyLabel really do end at the identical announce step: driving a
// general-namespace write through the bus request path and a
// reserved-namespace write through the direct SetReservedLabel/
// DeleteReservedLabel method must both produce a session.tag.set /
// session.tag.deleted announcement whose Key/Value/SessionID line up with
// what was actually requested, on both set and delete.
func TestSessionTagAnnounce_GeneralAndReservedPathsMatch(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	var setEvents []events.SessionTagSet
	var deletedEvents []events.SessionTagDeleted
	bus.Subscribe("session.tag.set", func(e Event[any]) {
		if v, ok := e.Payload.(events.SessionTagSet); ok {
			setEvents = append(setEvents, v)
		}
	})
	bus.Subscribe("session.tag.deleted", func(e Event[any]) {
		if v, ok := e.Payload.(events.SessionTagDeleted); ok {
			deletedEvents = append(deletedEvents, v)
		}
	})

	// General path: before:session.tag.set / before:session.tag.delete.
	genSetReq := &events.SessionTagSetRequest{SchemaVersion: events.SessionTagSetRequestVersion, Key: "tenant", Value: "acme"}
	if veto, err := bus.EmitVetoable("before:session.tag.set", genSetReq); err != nil || veto.Vetoed {
		t.Fatalf("general set: veto=%+v err=%v", veto, err)
	}
	genDelReq := &events.SessionTagDeleteRequest{SchemaVersion: events.SessionTagDeleteRequestVersion, Key: "tenant"}
	if veto, err := bus.EmitVetoable("before:session.tag.delete", genDelReq); err != nil || veto.Vetoed {
		t.Fatalf("general delete: veto=%+v err=%v", veto, err)
	}

	// Reserved path: SetReservedLabel / DeleteReservedLabel, direct Go calls.
	if err := ws.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}
	if err := ws.DeleteReservedLabel("_principal_id"); err != nil {
		t.Fatalf("DeleteReservedLabel: %v", err)
	}

	if len(setEvents) != 2 {
		t.Fatalf("expected 2 session.tag.set announcements (general + reserved), got %d: %+v", len(setEvents), setEvents)
	}
	if len(deletedEvents) != 2 {
		t.Fatalf("expected 2 session.tag.deleted announcements (general + reserved), got %d: %+v", len(deletedEvents), deletedEvents)
	}

	genSet, reservedSet := setEvents[0], setEvents[1]
	if genSet.Key != "tenant" || genSet.Value != "acme" || genSet.SessionID != ws.ID {
		t.Fatalf("general-path session.tag.set = %+v, want Key=tenant Value=acme SessionID=%s", genSet, ws.ID)
	}
	if reservedSet.Key != "_principal_id" || reservedSet.Value != "user-42" || reservedSet.SessionID != ws.ID {
		t.Fatalf("reserved-path session.tag.set = %+v, want Key=_principal_id Value=user-42 SessionID=%s", reservedSet, ws.ID)
	}
	// Both routes carry the identical shape: same struct type, same
	// SessionID, and Key/Value equal to whatever each path actually
	// requested — proving there is exactly one announce channel regardless
	// of which write path produced it.
	if genSet.SessionID != reservedSet.SessionID {
		t.Fatalf("general and reserved announces disagree on SessionID: %q vs %q", genSet.SessionID, reservedSet.SessionID)
	}

	genDeleted, reservedDeleted := deletedEvents[0], deletedEvents[1]
	if genDeleted.Key != "tenant" || genDeleted.SessionID != ws.ID {
		t.Fatalf("general-path session.tag.deleted = %+v, want Key=tenant SessionID=%s", genDeleted, ws.ID)
	}
	if reservedDeleted.Key != "_principal_id" || reservedDeleted.SessionID != ws.ID {
		t.Fatalf("reserved-path session.tag.deleted = %+v, want Key=_principal_id SessionID=%s", reservedDeleted, ws.ID)
	}
	if genDeleted.SessionID != reservedDeleted.SessionID {
		t.Fatalf("general and reserved delete-announces disagree on SessionID: %q vs %q", genDeleted.SessionID, reservedDeleted.SessionID)
	}
}

// TestSessionResume_LabelsRoundTrip_GeneralAndReserved confirms that both a
// general-namespace and a reserved-namespace label survive a real resume:
// LoadSessionWorkspace against the same on-disk session directory a prior
// process wrote metadata/session.json for — not just re-reading
// SessionMetadata() against the still-open workspace that wrote it.
func TestSessionResume_LabelsRoundTrip_GeneralAndReserved(t *testing.T) {
	root := t.TempDir()
	bus1 := NewEventBus()

	ws1, err := NewSessionWorkspace(root, bus1)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	eng1 := &Engine{Bus: bus1, Session: ws1}
	eng1.installSessionTagHandlers()

	genReq := &events.SessionTagSetRequest{SchemaVersion: events.SessionTagSetRequestVersion, Key: "project", Value: "widgets"}
	if veto, err := bus1.EmitVetoable("before:session.tag.set", genReq); err != nil || veto.Vetoed {
		t.Fatalf("general set before resume: veto=%+v err=%v", veto, err)
	}
	if err := ws1.SetReservedLabel("_principal_id", "user-99"); err != nil {
		t.Fatalf("SetReservedLabel before resume: %v", err)
	}

	// Simulate the process restarting: a fresh bus, a fresh
	// SessionWorkspace value loaded by ID from the same root, nothing
	// carried over in memory.
	bus2 := NewEventBus()
	ws2, err := LoadSessionWorkspace(root, ws1.ID, bus2)
	if err != nil {
		t.Fatalf("LoadSessionWorkspace: %v", err)
	}

	meta, err := ws2.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata after resume: %v", err)
	}
	if meta.Labels["project"] != "widgets" {
		t.Fatalf("Labels[project] after resume = %q, want widgets", meta.Labels["project"])
	}
	if meta.Labels["_principal_id"] != "user-99" {
		t.Fatalf("Labels[_principal_id] after resume = %q, want user-99", meta.Labels["_principal_id"])
	}

	// And the invariant still holds post-resume: the reserved key remains
	// unwritable through the general bus path on the resumed workspace,
	// using a fresh Engine/handler wiring exactly as a real restarted
	// process would have.
	eng2 := &Engine{Bus: bus2, Session: ws2}
	eng2.installSessionTagHandlers()

	var announced int
	bus2.Subscribe("session.tag.set", func(Event[any]) { announced++ })
	attackReq := &events.SessionTagSetRequest{SchemaVersion: events.SessionTagSetRequestVersion, Key: "_principal_id", Value: "attacker-after-resume"}
	veto, err := bus2.EmitVetoable("before:session.tag.set", attackReq)
	if err != nil {
		t.Fatalf("EmitVetoable after resume: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected reserved key to remain unwritable through the general path after resume")
	}
	if announced != 0 {
		t.Fatalf("expected no session.tag.set announcement post-resume attack, got %d", announced)
	}

	reloaded, err := ws2.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata after resume-attack: %v", err)
	}
	if reloaded.Labels["_principal_id"] != "user-99" {
		t.Fatalf("Labels[_principal_id] changed after vetoed post-resume attack: %q, want unchanged user-99", reloaded.Labels["_principal_id"])
	}
}

// TestSessionTagHandlers_EmissionSurface is this component's equivalent of
// the contract harness's AssertNoUndeclaredEmissions: installSessionTagHandlers
// is core engine wiring, not an engine.Plugin, so there is no
// Subscriptions()/Emissions() pair for pkg/testharness/contract to check
// directly. In its place, this captures every event type that crosses the
// bus across one full round of general-set, general-delete, reserved-set,
// reserved-delete (success and reserved-rejection cases both) and asserts
// the only events this component ever puts ON the bus, beyond the
// before:session.tag.* requests callers drive it with, are exactly
// session.tag.set and session.tag.deleted — the two announce types this
// file's doc comments describe as "the one channel anything watching
// session labels needs to subscribe to".
func TestSessionTagHandlers_EmissionSurface(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	requestTypes := map[string]bool{
		"before:session.tag.set":    true,
		"before:session.tag.delete": true,
	}
	// applyLabel calls SaveMeta, whose underlying WriteFile already announces
	// session.file.created / session.file.updated on its own — see the
	// brief's "don't double-announce" note and SessionWorkspace.AnnounceWrite.
	// That is a pre-existing, independently-declared emission of
	// SessionWorkspace itself, not something installSessionTagHandlers adds;
	// it is expected here and excluded from the undeclared-emission check for
	// the same reason the before:* requests are — it isn't part of the
	// contract this test is pinning.
	knownSideEffects := map[string]bool{
		"session.file.created": true,
		"session.file.updated": true,
	}
	const (
		announceSet     = "session.tag.set"
		announceDeleted = "session.tag.deleted"
	)
	declaredEmissions := map[string]bool{
		announceSet:     true,
		announceDeleted: true,
	}

	var mu sync.Mutex
	var observed []string
	bus.SubscribeAll(func(e Event[any]) {
		mu.Lock()
		observed = append(observed, e.Type)
		mu.Unlock()
	})

	// Successful general set, then a rejected reserved set.
	if veto, err := bus.EmitVetoable("before:session.tag.set", &events.SessionTagSetRequest{
		SchemaVersion: events.SessionTagSetRequestVersion, Key: "tenant", Value: "acme",
	}); err != nil || veto.Vetoed {
		t.Fatalf("general set: veto=%+v err=%v", veto, err)
	}
	if veto, err := bus.EmitVetoable("before:session.tag.set", &events.SessionTagSetRequest{
		SchemaVersion: events.SessionTagSetRequestVersion, Key: "_principal_id", Value: "nope",
	}); err != nil || !veto.Vetoed {
		t.Fatalf("reserved set: expected veto, got veto=%+v err=%v", veto, err)
	}
	// Successful general delete, then a rejected reserved delete.
	if err := ws.SetReservedLabel("_principal_id", "user-1"); err != nil {
		t.Fatalf("seed SetReservedLabel: %v", err)
	}
	if veto, err := bus.EmitVetoable("before:session.tag.delete", &events.SessionTagDeleteRequest{
		SchemaVersion: events.SessionTagDeleteRequestVersion, Key: "tenant",
	}); err != nil || veto.Vetoed {
		t.Fatalf("general delete: veto=%+v err=%v", veto, err)
	}
	if veto, err := bus.EmitVetoable("before:session.tag.delete", &events.SessionTagDeleteRequest{
		SchemaVersion: events.SessionTagDeleteRequestVersion, Key: "_principal_id",
	}); err != nil || !veto.Vetoed {
		t.Fatalf("reserved delete: expected veto, got veto=%+v err=%v", veto, err)
	}
	// The reserved path's own sanctioned write/delete.
	if err := ws.SetReservedLabel("_session_meta", "v1"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}
	if err := ws.DeleteReservedLabel("_session_meta"); err != nil {
		t.Fatalf("DeleteReservedLabel: %v", err)
	}

	mu.Lock()
	types := append([]string(nil), observed...)
	mu.Unlock()

	sawAnnounce := map[string]bool{}
	for _, ty := range types {
		if requestTypes[ty] || knownSideEffects[ty] {
			continue // caller-driven requests and SessionWorkspace's own pre-existing file announcements, not this component's emissions
		}
		sawAnnounce[ty] = true
		if !declaredEmissions[ty] {
			t.Errorf("session-tag handling put undeclared event type %q on the bus (declared: %v)", ty, declaredEmissions)
		}
	}
	if !sawAnnounce[announceSet] {
		t.Errorf("expected %q to have been observed on the bus", announceSet)
	}
	if !sawAnnounce[announceDeleted] {
		t.Errorf("expected %q to have been observed on the bus", announceDeleted)
	}
}
