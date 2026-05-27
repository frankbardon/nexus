package engine

import (
	"sync"
	"testing"
)

// TestCausation_AutoFillFromDispatchStack verifies that an event emitted from
// inside another event's handler automatically carries the outer event's ID
// and seq as its ParentID/ParentSeq.
func TestCausation_AutoFillFromDispatchStack(t *testing.T) {
	bus := NewEventBus().(*eventBus)

	var (
		mu      sync.Mutex
		parent  EventMeta
		child   EventMeta
		gotMeta bool
	)
	bus.Subscribe("parent.event", func(ev Event[any]) {
		mu.Lock()
		parent = ev.Meta()
		mu.Unlock()
		_ = bus.Emit("child.event", nil)
	})
	bus.Subscribe("child.event", func(ev Event[any]) {
		mu.Lock()
		child = ev.Meta()
		gotMeta = true
		mu.Unlock()
	})

	if err := bus.Emit("parent.event", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotMeta {
		t.Fatalf("child handler never ran")
	}
	if child.Causation.ParentID != parent.ID {
		t.Errorf("child.ParentID = %q, want %q", child.Causation.ParentID, parent.ID)
	}
	if child.Causation.ParentSeq != parent.Causation.Sequence {
		t.Errorf("child.ParentSeq = %d, want %d", child.Causation.ParentSeq, parent.Causation.Sequence)
	}
	if child.Causation.Sequence <= parent.Causation.Sequence {
		t.Errorf("child.Sequence = %d, want > parent.Sequence %d", child.Causation.Sequence, parent.Causation.Sequence)
	}
}

func TestCausation_DefaultContextAppliesSessionID(t *testing.T) {
	bus := NewEventBus().(*eventBus)
	bus.SetDefaultCausationContext(CausationContext{SessionID: "sess-1"})

	var got EventMeta
	bus.Subscribe("hello", func(ev Event[any]) {
		got = ev.Meta()
	})
	if err := bus.Emit("hello", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Causation.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.Causation.SessionID)
	}
	if got.Causation.Sequence == 0 {
		t.Errorf("Sequence not assigned")
	}
}

func TestCausation_PushedContextOverridesDefault(t *testing.T) {
	bus := NewEventBus().(*eventBus)
	bus.SetDefaultCausationContext(CausationContext{SessionID: "session-default"})

	pop := bus.PushCausationContext(CausationContext{
		SessionID: "session-pushed",
		AgentID:   "sub-agent-A",
		Depth:     2,
	})
	defer pop()

	var got EventMeta
	bus.Subscribe("scoped", func(ev Event[any]) {
		got = ev.Meta()
	})
	if err := bus.Emit("scoped", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Causation.SessionID != "session-pushed" {
		t.Errorf("SessionID = %q, want session-pushed", got.Causation.SessionID)
	}
	if got.Causation.AgentID != "sub-agent-A" {
		t.Errorf("AgentID = %q, want sub-agent-A", got.Causation.AgentID)
	}
	if got.Causation.Depth != 2 {
		t.Errorf("Depth = %d, want 2", got.Causation.Depth)
	}
}

func TestCausation_RespectsExplicitCallerFields(t *testing.T) {
	bus := NewEventBus().(*eventBus)
	bus.SetDefaultCausationContext(CausationContext{SessionID: "auto"})

	var got EventMeta
	bus.Subscribe("preserved", func(ev Event[any]) {
		got = ev.Meta()
	})

	ev := Event[any]{
		Type: "preserved",
		Causation: EventCausation{
			SessionID: "manual",
			AgentID:   "manual-agent",
			ParentID:  "manual-parent",
			ParentSeq: 42,
		},
	}
	if err := bus.EmitEvent(ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got.Causation.SessionID != "manual" {
		t.Errorf("SessionID overridden: %q", got.Causation.SessionID)
	}
	if got.Causation.AgentID != "manual-agent" {
		t.Errorf("AgentID overridden: %q", got.Causation.AgentID)
	}
	if got.Causation.ParentID != "manual-parent" {
		t.Errorf("ParentID overridden: %q", got.Causation.ParentID)
	}
	if got.Causation.ParentSeq != 42 {
		t.Errorf("ParentSeq overridden: %d", got.Causation.ParentSeq)
	}
}
