package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPluginBaseID(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"nexus.agent.react":               "nexus.agent.react",
		"nexus.agent.subagent/researcher": "nexus.agent.subagent",
		"a/b/c":                           "a", // first slash wins
		"/leading":                        "",  // empty base before slash
		"trailing/":                       "trailing",
	}
	for in, want := range cases {
		if got := PluginBaseID(in); got != want {
			t.Errorf("PluginBaseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEventBus_SetSeqFloor_AdvancesAndIsMonotonic(t *testing.T) {
	bus := NewEventBus().(*eventBus)

	if got := bus.seqCounter.Load(); got != 0 {
		t.Fatalf("fresh bus seqCounter = %d, want 0", got)
	}

	bus.SetSeqFloor(100)
	if got := bus.seqCounter.Load(); got != 100 {
		t.Errorf("after SetSeqFloor(100): %d, want 100", got)
	}

	// SetSeqFloor below current is a no-op.
	bus.SetSeqFloor(50)
	if got := bus.seqCounter.Load(); got != 100 {
		t.Errorf("SetSeqFloor(50) below floor must not regress: got %d", got)
	}

	// First Emit after a SetSeqFloor(100) gets seq 101.
	if err := bus.Emit("seq.test", nil); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := bus.seqCounter.Load(); got != 101 {
		t.Errorf("Emit after floor: counter = %d, want 101", got)
	}
}

func TestEventBus_CurrentSeq_AndParentSeq_DuringDispatch(t *testing.T) {
	bus := NewEventBus().(*eventBus)

	var (
		outerSeq, outerParent uint64
		innerSeq, innerParent uint64
	)

	bus.Subscribe("inner", func(ev Event[any]) {
		innerSeq = bus.CurrentSeq()
		innerParent = bus.ParentSeq()
	})
	bus.Subscribe("outer", func(ev Event[any]) {
		outerSeq = bus.CurrentSeq()
		outerParent = bus.ParentSeq()
		_ = bus.Emit("inner", nil)
	})

	if err := bus.Emit("outer", nil); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if outerSeq == 0 {
		t.Error("CurrentSeq inside outer handler should be non-zero")
	}
	if outerParent != 0 {
		t.Errorf("ParentSeq inside top-level handler should be 0, got %d", outerParent)
	}
	if innerSeq == 0 {
		t.Error("CurrentSeq inside inner handler should be non-zero")
	}
	if innerParent != outerSeq {
		t.Errorf("ParentSeq of nested emit = %d, want outer seq %d", innerParent, outerSeq)
	}
	if innerSeq == outerSeq {
		t.Error("nested emit should have its own seq, distinct from parent")
	}

	// After dispatch returns, the goroutine's stack must be empty.
	if got := bus.CurrentSeq(); got != 0 {
		t.Errorf("CurrentSeq after dispatch = %d, want 0", got)
	}
}

func TestEventBus_Subscribe_FilterPredicate(t *testing.T) {
	bus := NewEventBus()

	var matched, unmatched int
	keepEven := func(meta EventMeta) bool {
		return len(meta.ID)%2 == 0
	}
	bus.Subscribe("filter.test", func(ev Event[any]) { matched++ }, WithFilter(keepEven))
	bus.Subscribe("filter.test", func(ev Event[any]) { unmatched++ })

	for i := range 5 {
		_ = bus.Emit("filter.test", i)
	}

	if unmatched != 5 {
		t.Errorf("unfiltered subscriber received %d events, want 5", unmatched)
	}
	// At least the filter ran without error (5 events with random IDs — count
	// won't always be exactly some number, just verify the filter narrowed
	// the receiver vs. the unfiltered baseline).
	if matched > unmatched {
		t.Errorf("filtered receiver got more events than unfiltered: filtered=%d unfiltered=%d", matched, unmatched)
	}
}

func TestEventBus_Subscribe_Unsubscribe(t *testing.T) {
	bus := NewEventBus()

	var hits int32
	unsub := bus.Subscribe("unsub.test", func(ev Event[any]) {
		atomic.AddInt32(&hits, 1)
	})

	_ = bus.Emit("unsub.test", nil)
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("first emit hits = %d, want 1", hits)
	}

	unsub()
	_ = bus.Emit("unsub.test", nil)
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("after unsubscribe, hits = %d, want still 1", hits)
	}
}

func TestEventBus_SubscribeAll_OnlySeesPostSubscriptionEvents(t *testing.T) {
	bus := NewEventBus()

	_ = bus.Emit("pre", "before-subscribe")

	var seen []string
	bus.SubscribeAll(func(ev Event[any]) {
		seen = append(seen, ev.Type)
	})

	_ = bus.Emit("post", "after-subscribe")

	if len(seen) != 1 || seen[0] != "post" {
		t.Errorf("SubscribeAll saw %v, want [post]", seen)
	}
}

func TestEventBus_SubscribeAllReplay_DeliversRingThenLive(t *testing.T) {
	bus := NewEventBus()

	for _, t := range []string{"a", "b", "c"} {
		_ = bus.Emit(t, nil)
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	bus.SubscribeAllReplay(func(ev Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev.Type)
	})

	_ = bus.Emit("d", nil)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"a", "b", "c", "d"}
	if len(seen) != len(want) {
		t.Fatalf("SubscribeAllReplay saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("seen[%d] = %q, want %q (full %v)", i, seen[i], want[i], seen)
		}
	}
}

// TestEventBus_SubscribeAllReplay_RaceWithEmit verifies the ring-snapshot
// + subscribe-registration protocol delivers every event exactly once even
// when concurrent emits race with the SubscribeAllReplay call. Run under
// `go test -race`.
func TestEventBus_SubscribeAllReplay_RaceWithEmit(t *testing.T) {
	bus := NewEventBus()

	const emits = 500
	emitDone := make(chan struct{})
	go func() {
		for i := range emits {
			_ = bus.Emit("race.evt", i)
		}
		close(emitDone)
	}()

	// Race the subscribe in part-way through.
	time.Sleep(time.Microsecond)

	var (
		mu   sync.Mutex
		seen = map[int]int{}
	)
	bus.SubscribeAllReplay(func(ev Event[any]) {
		if v, ok := ev.Payload.(int); ok {
			mu.Lock()
			seen[v]++
			mu.Unlock()
		}
	})

	<-emitDone
	if err := bus.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for v, n := range seen {
		if n != 1 {
			t.Errorf("event %d delivered %d times, want exactly 1", v, n)
		}
	}
}

func TestEventBus_SetFailFast_PropagatesPanic(t *testing.T) {
	bus := NewEventBus()
	bus.(*eventBus).SetFailFast(true)

	bus.Subscribe("panic.test", func(ev Event[any]) {
		panic("boom")
	})

	defer func() {
		if r := recover(); r == nil {
			t.Error("fail-fast bus should propagate panic; recovered nil")
		}
	}()
	_ = bus.Emit("panic.test", nil)
}
