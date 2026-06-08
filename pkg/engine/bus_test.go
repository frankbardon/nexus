package engine

import (
	"context"
	"log/slog"
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

// Equal-priority handlers must run in subscription order — the first
// Subscribe at a given priority runs first. This is the deterministic
// tiebreak that issue #121 introduces; before the fix, sort.Slice could
// reorder them on every Subscribe call, silently flipping which veto wins.
func TestEmitVetoable_StableTiebreakBySubscriptionOrder(t *testing.T) {
	bus := NewEventBus()

	var order []string

	unsubA := bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, "A")
	}, WithPriority(10))
	bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, "B")
	}, WithPriority(10))
	bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, "C")
	}, WithPriority(10))

	msg := "test"
	_, _ = bus.EmitVetoable("before:test.action", &msg)

	if len(order) != 3 || order[0] != "A" || order[1] != "B" || order[2] != "C" {
		t.Fatalf("expected order [A B C], got %v", order)
	}

	// Unsubscribe B then add D. Stable sort must keep [A, C] in registration
	// order; D appended afterward and re-sorted should land at the end.
	unsubA() // also exercise unsubscribe path on a tied-priority sub
	bus.Subscribe("before:test.action", func(e Event[any]) {
		order = append(order, "D")
	}, WithPriority(10))

	order = nil
	_, _ = bus.EmitVetoable("before:test.action", &msg)
	if len(order) != 3 || order[0] != "B" || order[1] != "C" || order[2] != "D" {
		t.Fatalf("expected order [B C D] after unsub A + add D, got %v", order)
	}
}

// WarnVetoableCollisions must surface (event, priority) tuples that have
// 2+ subscribers on a before:* event, and ignore non-vetoable events
// regardless of duplicates.
func TestBus_WarnVetoableCollisions(t *testing.T) {
	bus := NewEventBus().(*eventBus)

	rec := &recordingHandler{}
	bus.SetLogger(slog.New(rec))

	bus.Subscribe("before:io.output", func(Event[any]) {}, WithPriority(10), WithSource("plugin.a"))
	bus.Subscribe("before:io.output", func(Event[any]) {}, WithPriority(10), WithSource("plugin.b"))
	bus.Subscribe("before:io.output", func(Event[any]) {}, WithPriority(12), WithSource("plugin.c")) // no collision
	// Non-vetoable duplicate — must be ignored.
	bus.Subscribe("tool.invoke", func(Event[any]) {}, WithPriority(50), WithSource("plugin.d"))
	bus.Subscribe("tool.invoke", func(Event[any]) {}, WithPriority(50), WithSource("plugin.e"))

	bus.WarnVetoableCollisions()

	var hits []slog.Record
	for _, r := range rec.records {
		if r.Level == slog.LevelWarn {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 WARN, got %d (records: %+v)", len(hits), rec.records)
	}

	got := recordAttrs(hits[0])
	if got["event"] != "before:io.output" {
		t.Errorf("event = %v, want before:io.output", got["event"])
	}
	if got["priority"] != int64(10) {
		t.Errorf("priority = %v, want 10", got["priority"])
	}
	sources, ok := got["sources"].([]string)
	if !ok {
		t.Fatalf("sources missing or wrong type: %T %v", got["sources"], got["sources"])
	}
	if len(sources) != 2 || sources[0] != "plugin.a" || sources[1] != "plugin.b" {
		t.Errorf("sources = %v, want [plugin.a plugin.b]", sources)
	}
}

type recordingHandler struct {
	records []slog.Record
}

func (r *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (r *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	r.records = append(r.records, rec)
	return nil
}
func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingHandler) WithGroup(string) slog.Handler      { return r }

func recordAttrs(r slog.Record) map[string]any {
	out := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Value.Kind() {
		case slog.KindInt64:
			out[a.Key] = a.Value.Int64()
		case slog.KindAny:
			out[a.Key] = a.Value.Any()
		default:
			out[a.Key] = a.Value.Any()
		}
		return true
	})
	return out
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

// TestBus_ExcludedTypeSkipsSeqAndRing verifies that an excluded event type
// (the journal's core.tick filter) skips seq assignment, the replay ring,
// and dispatch-stack tracking, while still being delivered to typed and
// wildcard subscribers. This is the contract startJournal relies on to
// keep on-disk envelopes gap-free.
func TestBus_ExcludedTypeSkipsSeqAndRing(t *testing.T) {
	bus := NewEventBus()
	ec, ok := bus.(ExcludeController)
	if !ok {
		t.Fatal("eventBus does not implement ExcludeController")
	}
	ec.SetExcludedTypes([]string{"core.tick"})

	seqSrc, ok := bus.(interface {
		CurrentSeq() uint64
		ParentSeq() uint64
	})
	if !ok {
		t.Fatal("eventBus does not expose CurrentSeq/ParentSeq")
	}

	var wildcardSeen []string
	var wildcardSeqs []uint64
	bus.SubscribeAll(func(e Event[any]) {
		wildcardSeen = append(wildcardSeen, e.Type)
		wildcardSeqs = append(wildcardSeqs, seqSrc.CurrentSeq())
	})

	var tickHits int
	bus.Subscribe("core.tick", func(e Event[any]) { tickHits++ })

	// Interleave excluded and non-excluded events.
	for _, evt := range []string{"core.tick", "foo", "core.tick", "bar", "baz", "core.tick"} {
		if err := bus.Emit(evt, nil); err != nil {
			t.Fatalf("emit %s: %v", evt, err)
		}
	}

	// Wildcard must see every event (excluded events still dispatch).
	if got, want := len(wildcardSeen), 6; got != want {
		t.Fatalf("wildcard saw %d events, want %d (%v)", got, want, wildcardSeen)
	}
	// Typed subscribers for excluded types still fire.
	if tickHits != 3 {
		t.Fatalf("core.tick typed handler fired %d times, want 3", tickHits)
	}

	// During an excluded event's dispatch, CurrentSeq must be 0 (no
	// dispatch-stack push). During a non-excluded event, CurrentSeq must
	// be the bus-assigned monotonic seq starting at 1 with no gaps for
	// the excluded ones.
	wantSeqs := []uint64{0, 1, 0, 2, 3, 0}
	if len(wildcardSeqs) != len(wantSeqs) {
		t.Fatalf("seq count mismatch: got %v want %v", wildcardSeqs, wantSeqs)
	}
	for i, s := range wildcardSeqs {
		if s != wantSeqs[i] {
			t.Errorf("wildcard seq[%d] type=%s got %d want %d", i, wildcardSeen[i], s, wantSeqs[i])
		}
	}

	// Clearing the exclusion list restores seq assignment.
	ec.SetExcludedTypes(nil)
	if ec.IsExcluded("core.tick") {
		t.Fatal("IsExcluded(core.tick) still true after clearing")
	}
	wildcardSeqs = wildcardSeqs[:0]
	wildcardSeen = wildcardSeen[:0]
	if err := bus.Emit("core.tick", nil); err != nil {
		t.Fatalf("emit after clear: %v", err)
	}
	if len(wildcardSeqs) != 1 || wildcardSeqs[0] != 4 {
		t.Fatalf("after clear, expected seq 4 for core.tick; got %v", wildcardSeqs)
	}
}
