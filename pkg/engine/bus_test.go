package engine

import (
	"testing"
)

func TestEmitVetoable_HandlerCanVeto(t *testing.T) {
	bus := NewEventBus()

	bus.Subscribe("before:test.action", func(e Event[any]) {
		vp, ok := e.Payload.(*VetoablePayload)
		if !ok {
			t.Fatal("payload is not *VetoablePayload")
		}
		// Inspect original payload.
		orig, ok := vp.Original.(*string)
		if !ok {
			t.Fatal("original payload is not *string")
		}
		if *orig == "bad" {
			vp.Veto = VetoResult{Vetoed: true, Reason: "content is bad"}
		}
	})

	// Should veto.
	bad := "bad"
	result, err := bus.EmitVetoable("before:test.action", &bad)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed {
		t.Fatal("expected veto, got pass")
	}
	if result.Reason != "content is bad" {
		t.Fatalf("expected reason 'content is bad', got %q", result.Reason)
	}

	// Should pass.
	good := "good"
	result, err = bus.EmitVetoable("before:test.action", &good)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vetoed {
		t.Fatal("expected pass, got veto")
	}
}

func TestEmitVetoable_NoHandlers_NeverVetoes(t *testing.T) {
	bus := NewEventBus()

	msg := "anything"
	result, err := bus.EmitVetoable("before:test.action", &msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vetoed {
		t.Fatal("expected pass with no handlers")
	}
}

func TestEmitVetoable_FirstVetoWins(t *testing.T) {
	bus := NewEventBus()

	// Handler at priority 10 vetoes.
	bus.Subscribe("before:test.action", func(e Event[any]) {
		vp := e.Payload.(*VetoablePayload)
		vp.Veto = VetoResult{Vetoed: true, Reason: "first"}
	}, WithPriority(10))

	// Handler at priority 20 should never run.
	ran := false
	bus.Subscribe("before:test.action", func(e Event[any]) {
		ran = true
	}, WithPriority(20))

	msg := "test"
	result, err := bus.EmitVetoable("before:test.action", &msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Vetoed || result.Reason != "first" {
		t.Fatalf("expected first handler veto, got %+v", result)
	}
	if ran {
		t.Fatal("second handler should not run after veto")
	}
}

func TestEmitVetoable_PriorityOrder(t *testing.T) {
	bus := NewEventBus()

	var order []int

	bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, 2)
	}, WithPriority(20))

	bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, 1)
	}, WithPriority(10))

	msg := "test"
	_, _ = bus.EmitVetoable("before:test.action", &msg)

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("expected order [1, 2], got %v", order)
	}
}

func TestEmitVetoable_OriginalPayloadAccessible(t *testing.T) {
	bus := NewEventBus()

	type TestPayload struct {
		Name  string
		Value int
	}

	bus.Subscribe("before:test.action", func(e Event[any]) {
		vp := e.Payload.(*VetoablePayload)
		orig, ok := vp.Original.(*TestPayload)
		if !ok {
			t.Fatal("could not cast original payload")
		}
		if orig.Name != "test" || orig.Value != 42 {
			t.Fatalf("unexpected original: %+v", orig)
		}
	})

	payload := &TestPayload{Name: "test", Value: 42}
	result, err := bus.EmitVetoable("before:test.action", payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vetoed {
		t.Fatal("should not veto")
	}
}

func TestEmitVetoable_CanModifyOriginalPayload(t *testing.T) {
	bus := NewEventBus()

	type TestPayload struct {
		Content string
	}

	bus.Subscribe("before:test.action", func(e Event[any]) {
		vp := e.Payload.(*VetoablePayload)
		orig := vp.Original.(*TestPayload)
		// Redact in-place without vetoing (content safety "redact" mode).
		orig.Content = "[REDACTED]"
	})

	payload := &TestPayload{Content: "secret-key-12345"}
	result, _ := bus.EmitVetoable("before:test.action", payload)
	if result.Vetoed {
		t.Fatal("should not veto on redact")
	}
	if payload.Content != "[REDACTED]" {
		t.Fatalf("expected redacted content, got %q", payload.Content)
	}
}
