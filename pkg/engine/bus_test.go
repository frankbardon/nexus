package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestEmitAsync_RunsHandlerConcurrently(t *testing.T) {
	bus := NewEventBus()

	var called atomic.Bool
	bus.Subscribe("test.async", func(e Event[any]) {
		called.Store(true)
	})

	ch := bus.EmitAsync("test.async", "hello")

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EmitAsync did not complete in time")
	}

	if !called.Load() {
		t.Fatal("handler was not called")
	}
}

func TestEmitAsync_ChannelCloses(t *testing.T) {
	bus := NewEventBus()

	ch := bus.EmitAsync("test.noop", nil)

	select {
	case _, open := <-ch:
		// First read gets nil error.
		if open {
			// Channel should close after.
			_, open = <-ch
			if open {
				t.Fatal("channel should be closed after result")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EmitAsync channel did not close in time")
	}
}

func TestNewEventBus_DefaultsToEventRingSize(t *testing.T) {
	bus := NewEventBus().(*eventBus)
	if bus.ring.cap != DefaultEventRingSize {
		t.Errorf("ring cap = %d, want %d", bus.ring.cap, DefaultEventRingSize)
	}
	if DefaultEventRingSize >= DefaultLogRingSize {
		t.Errorf("DefaultEventRingSize (%d) should be smaller than DefaultLogRingSize (%d) — ring is for boot gap only",
			DefaultEventRingSize, DefaultLogRingSize)
	}
}

func TestNewEventBusWithRingSize_HonorsExplicitCap(t *testing.T) {
	bus := NewEventBusWithRingSize(128).(*eventBus)
	if bus.ring.cap != 128 {
		t.Errorf("ring cap = %d, want 128", bus.ring.cap)
	}
}

func TestNewEventBusWithRingSize_FallsBackOnZero(t *testing.T) {
	bus := NewEventBusWithRingSize(0).(*eventBus)
	if bus.ring.cap != DefaultEventRingSize {
		t.Errorf("ring cap = %d, want default %d", bus.ring.cap, DefaultEventRingSize)
	}
}

// TestDrain_ConcurrentEmitsDoNotPanic exercises the race that triggered
// "WaitGroup is reused before previous Wait has returned" in CI: many
// emits firing concurrently with a Drain call. Without the drainMu
// guard around inflight.Add, this test panics under -race within a
// few iterations.
func TestDrain_ConcurrentEmitsDoNotPanic(t *testing.T) {
	bus := NewEventBus()

	const workers = 32
	const emitsPerWorker = 200

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < emitsPerWorker; j++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = bus.Emit("test.event", j)
			}
		}()
	}

	// Drain repeatedly while emits are flying. Each Drain may finish
	// either because all in-flight emits completed or because new
	// emits keep arriving and we eventually time out — both are fine
	// for the regression case; the only failure mode we care about
	// is the runtime panic.
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_ = bus.Drain(ctx)
		cancel()
	}

	close(stop)
	wg.Wait()

	// Final clean drain so the bus settles before the test exits.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bus.Drain(ctx); err != nil {
		t.Fatalf("final Drain: %v", err)
	}
}
