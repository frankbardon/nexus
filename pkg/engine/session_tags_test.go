package engine

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// newTagTestEngine builds a bus + session workspace, wires
// installSessionTagHandlers against them, and hands back all three so a test
// can both drive the bus and inspect Labels on disk. Deliberately not a full
// Engine.New + Boot: installSessionTagHandlers only touches e.Bus and
// e.Session, so the rest of Boot (journal, plugin registry, object store,
// ...) would be unexercised setup cost for these tests. reload_test.go's
// newReloadEngine established the same "stitch an Engine by hand" pattern
// for ReloadConfig.
func newTagTestEngine(t *testing.T) (*SessionWorkspace, EventBus) {
	t.Helper()
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	eng := &Engine{Bus: bus, Session: ws}
	eng.installSessionTagHandlers()
	return ws, bus
}

func TestIsReservedLabelKey(t *testing.T) {
	cases := map[string]bool{
		"_principal_id": true,
		"_":             true,
		"tenant":        false,
		"":              false,
		"a_b":           false,
	}
	for key, want := range cases {
		if got := IsReservedLabelKey(key); got != want {
			t.Errorf("IsReservedLabelKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// The core enforcement point: a reserved key on the general bus path must be
// vetoed and must never reach Labels, regardless of who emitted the request.
func TestBeforeSessionTagSet_ReservedKeyRejected(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	var announced int
	bus.Subscribe("session.tag.set", func(Event[any]) { announced++ })

	req := &events.SessionTagSetRequest{SchemaVersion: events.SessionTagSetRequestVersion, Key: "_principal_id", Value: "abc"}
	veto, err := bus.EmitVetoable("before:session.tag.set", req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected reserved key to be vetoed")
	}
	if veto.Reason == "" {
		t.Error("expected a non-empty veto reason")
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; ok {
		t.Fatal("reserved key was written to Labels despite veto")
	}
	if announced != 0 {
		t.Fatalf("expected no session.tag.set announcement, got %d", announced)
	}
}

// A non-reserved key on the general bus path succeeds: persisted via
// SaveMeta, and the announce event carries the write.
func TestBeforeSessionTagSet_GeneralKeySucceeds(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	var got events.SessionTagSet
	var fired bool
	bus.Subscribe("session.tag.set", func(e Event[any]) {
		fired = true
		got, _ = e.Payload.(events.SessionTagSet)
	})

	req := &events.SessionTagSetRequest{SchemaVersion: events.SessionTagSetRequestVersion, Key: "tenant", Value: "acme"}
	veto, err := bus.EmitVetoable("before:session.tag.set", req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("expected no veto, got reason %q", veto.Reason)
	}
	if !fired {
		t.Fatal("expected session.tag.set to fire")
	}
	if got.Key != "tenant" || got.Value != "acme" || got.SessionID != ws.ID {
		t.Fatalf("announce payload = %+v", got)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels["tenant"] != "acme" {
		t.Fatalf("Labels[tenant] = %q, want acme", meta.Labels["tenant"])
	}
}

func TestBeforeSessionTagDelete_ReservedKeyRejected(t *testing.T) {
	ws, bus := newTagTestEngine(t)
	// Seed the reserved key via the sanctioned direct path so delete has
	// something to (fail to) remove.
	if err := ws.SetReservedLabel("_principal_id", "abc"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	req := &events.SessionTagDeleteRequest{SchemaVersion: events.SessionTagDeleteRequestVersion, Key: "_principal_id"}
	veto, err := bus.EmitVetoable("before:session.tag.delete", req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected reserved key delete to be vetoed")
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; !ok {
		t.Fatal("reserved key was deleted despite veto")
	}
}

func TestBeforeSessionTagDelete_GeneralKeySucceeds(t *testing.T) {
	ws, bus := newTagTestEngine(t)
	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	meta.Labels["project"] = "widgets"
	if err := ws.SaveMeta(meta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	var got events.SessionTagDeleted
	var fired bool
	bus.Subscribe("session.tag.deleted", func(e Event[any]) {
		fired = true
		got, _ = e.Payload.(events.SessionTagDeleted)
	})

	req := &events.SessionTagDeleteRequest{SchemaVersion: events.SessionTagDeleteRequestVersion, Key: "project"}
	veto, err := bus.EmitVetoable("before:session.tag.delete", req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("expected no veto, got reason %q", veto.Reason)
	}
	if !fired {
		t.Fatal("expected session.tag.deleted to fire")
	}
	if got.Key != "project" || got.SessionID != ws.ID {
		t.Fatalf("announce payload = %+v", got)
	}

	reloaded, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := reloaded.Labels["project"]; ok {
		t.Fatal("expected project label to be removed")
	}
}

func TestSetReservedLabel_SetsAndAnnounces(t *testing.T) {
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}

	var got events.SessionTagSet
	var fired bool
	bus.Subscribe("session.tag.set", func(e Event[any]) {
		fired = true
		got, _ = e.Payload.(events.SessionTagSet)
	})

	if err := ws.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}
	if !fired {
		t.Fatal("expected session.tag.set to fire for a reserved write")
	}
	if got.Key != "_principal_id" || got.Value != "user-42" {
		t.Fatalf("announce payload = %+v", got)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels["_principal_id"] != "user-42" {
		t.Fatalf("Labels[_principal_id] = %q, want user-42", meta.Labels["_principal_id"])
	}
}

func TestSetReservedLabel_RejectsNonReservedKey(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if err := ws.SetReservedLabel("tenant", "acme"); err == nil {
		t.Fatal("expected error setting a non-reserved key via SetReservedLabel")
	}
	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := meta.Labels["tenant"]; ok {
		t.Fatal("SetReservedLabel wrote a non-reserved key despite rejecting it")
	}
}

func TestDeleteReservedLabel_SetsAndAnnounces(t *testing.T) {
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	if err := ws.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	var got events.SessionTagDeleted
	var fired bool
	bus.Subscribe("session.tag.deleted", func(e Event[any]) {
		fired = true
		got, _ = e.Payload.(events.SessionTagDeleted)
	})

	if err := ws.DeleteReservedLabel("_principal_id"); err != nil {
		t.Fatalf("DeleteReservedLabel: %v", err)
	}
	if !fired {
		t.Fatal("expected session.tag.deleted to fire for a reserved delete")
	}
	if got.Key != "_principal_id" {
		t.Fatalf("announce payload = %+v", got)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; ok {
		t.Fatal("expected _principal_id label to be removed")
	}
}

func TestDeleteReservedLabel_RejectsNonReservedKey(t *testing.T) {
	ws, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	meta.Labels["tenant"] = "acme"
	if err := ws.SaveMeta(meta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	if err := ws.DeleteReservedLabel("tenant"); err == nil {
		t.Fatal("expected error deleting a non-reserved key via DeleteReservedLabel")
	}
	reloaded, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if reloaded.Labels["tenant"] != "acme" {
		t.Fatal("DeleteReservedLabel removed a non-reserved key despite rejecting it")
	}
}
