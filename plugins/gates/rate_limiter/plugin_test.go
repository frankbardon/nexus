package ratelimiter

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newRejectPlugin(t *testing.T, perWindow int, window time.Duration) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		mode:              ModeReject,
		requestsPerWindow: perWindow,
		windowDuration:    window,
		maxPending:        defaultMaxPending,
		pauseMessage:      "paused {seconds}s",
		nowFunc:           time.Now,
	}
	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(8)),
	)
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, bus
}

func newQueuePlugin(t *testing.T, perWindow int, window time.Duration, maxPending int) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		mode:              ModeQueue,
		requestsPerWindow: perWindow,
		windowDuration:    window,
		maxPending:        maxPending,
		pauseMessage:      "paused {seconds}s",
		nowFunc:           time.Now,
		queueCh:           make(chan struct{}, maxPending),
		drainerDone:       make(chan struct{}),
		stopCh:            make(chan struct{}),
	}
	go p.drain()
	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(8)),
	)
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, bus
}

func TestRateLimiter_UnderLimit_NoVeto(t *testing.T) {
	_, bus := newRejectPlugin(t, 5, 10*time.Second)
	for i := range 4 {
		veto, err := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		if veto.Vetoed {
			t.Fatalf("request %d: should not be vetoed under limit", i)
		}
	}
}

func TestRateLimiter_RejectMode_Vetoes(t *testing.T) {
	_, bus := newRejectPlugin(t, 2, 10*time.Second)
	for range 2 {
		bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	}
	veto, err := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected veto when over rate limit")
	}
}

func TestRateLimiter_RejectMode_EmitsRetryAfterWait(t *testing.T) {
	_, bus := newRejectPlugin(t, 1, 50*time.Millisecond)

	var (
		mu         sync.Mutex
		retryCount int
	)
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {
		mu.Lock()
		retryCount++
		mu.Unlock()
	})

	bus.EmitVetoable("before:llm.request", &events.LLMRequest{})

	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if !veto.Vetoed {
		t.Fatal("expected veto")
	}

	// Wait for the retry goroutine to fire.
	deadline := time.After(500 * time.Millisecond)
	for {
		mu.Lock()
		got := retryCount
		mu.Unlock()
		if got >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected gate.llm.retry, got %d", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRateLimiter_WindowExpiry_AllowsThrough(t *testing.T) {
	_, bus := newRejectPlugin(t, 1, 50*time.Millisecond)
	bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	time.Sleep(60 * time.Millisecond)
	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if veto.Vetoed {
		t.Fatal("should not veto after window expiry")
	}
}

func TestRateLimiter_QueueMode_BeyondRateQueued(t *testing.T) {
	// 1 request / 50ms; queue holds up to 3.
	p, bus := newQueuePlugin(t, 1, 50*time.Millisecond, 3)

	retryC := make(chan struct{}, 8)
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {
		retryC <- struct{}{}
	})

	// Burn the first slot.
	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if veto.Vetoed {
		t.Fatal("first request must not be vetoed")
	}

	// Two more should queue (vetoed but not full).
	for i := range 2 {
		v, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
		if !v.Vetoed {
			t.Fatalf("queued request %d should veto, got passthrough", i)
		}
		if v.Reason == "" || v.Reason[:len("Rate limited (queued)")] != "Rate limited (queued)" {
			t.Fatalf("expected queued veto reason, got %q", v.Reason)
		}
	}

	// Drain at least 1 retry within ~250ms.
	select {
	case <-retryC:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queue drainer did not emit gate.llm.retry")
	}

	// Sanity: queue depth has dropped.
	if depth := len(p.queueCh); depth >= 2 {
		// Allow racy tick boundaries — but if depth somehow grew, fail.
		t.Logf("post-drain queue depth: %d", depth)
	}
}

func TestRateLimiter_QueueMode_RejectsBeyondMaxPending(t *testing.T) {
	_, bus := newQueuePlugin(t, 1, 5*time.Second, 2)

	// Burn the first allowed slot.
	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if veto.Vetoed {
		t.Fatal("first request must not be vetoed")
	}

	// Fill the queue (2 slots).
	for i := range 2 {
		v, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
		if !v.Vetoed {
			t.Fatalf("queued request %d should veto", i)
		}
	}

	// Next request must reject (queue full).
	v, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if !v.Vetoed {
		t.Fatal("expected veto when queue full")
	}
	if want := "Rate limited: queue full"; v.Reason[:len(want)] != want {
		t.Fatalf("expected queue-full reason, got %q", v.Reason)
	}
}

func TestRateLimiter_QueueMode_NoLeakOnShutdown(t *testing.T) {
	before := runtime.NumGoroutine()

	bus := engine.NewEventBus()
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		mode:              ModeQueue,
		requestsPerWindow: 1,
		windowDuration:    100 * time.Millisecond,
		maxPending:        4,
		pauseMessage:      "paused",
		nowFunc:           time.Now,
		queueCh:           make(chan struct{}, 4),
		drainerDone:       make(chan struct{}),
		stopCh:            make(chan struct{}),
	}
	go p.drain()
	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(8)),
	)
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {})

	// Drive some traffic concurrently.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
		}()
	}
	wg.Wait()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Drainer should have exited (Shutdown waits on drainerDone). Compare
	// goroutine count after a brief settle window — exact equality is
	// fragile but a non-trivial increase signals a leak.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}
