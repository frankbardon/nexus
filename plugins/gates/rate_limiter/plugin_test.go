package ratelimiter

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func TestRateLimiter_UnderLimit_NoVeto(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		requestsPerWindow: 5,
		windowDuration:    10 * time.Second,
		pauseMessage:      "paused {seconds}s",
		nowFunc:           time.Now,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8)),
	)

	// Fire 4 requests — all under the limit of 5.
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

func TestRateLimiter_OverLimit_Vetoes(t *testing.T) {
	bus := engine.NewEventBus()
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		requestsPerWindow: 2,
		windowDuration:    10 * time.Second,
		pauseMessage:      "paused {seconds}s",
		nowFunc:           time.Now,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8)),
	)

	// Fill the window.
	for range 2 {
		bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	}

	// Third request should be vetoed.
	veto, err := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected veto when over rate limit")
	}
}

func TestRateLimiter_EmitsRetryAfterWait(t *testing.T) {
	bus := engine.NewEventBus()

	now := time.Now()
	callCount := 0
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		requestsPerWindow: 1,
		windowDuration:    100 * time.Millisecond, // short window for test speed
		pauseMessage:      "paused {seconds}s",
		nowFunc: func() time.Time {
			callCount++
			// First call (recording): return fixed time
			// Second call (checking): return same time so window is full
			// Third call (after sleep, recording): return future time
			if callCount <= 2 {
				return now
			}
			return now.Add(200 * time.Millisecond)
		},
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8)),
	)

	// Track retry events.
	var mu sync.Mutex
	retryCount := 0
	bus.Subscribe("gate.llm.retry", func(_ engine.Event[any]) {
		mu.Lock()
		retryCount++
		mu.Unlock()
	})

	// Suppress io.output from causing issues.
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})

	// Fill window.
	bus.EmitVetoable("before:llm.request", &events.LLMRequest{})

	// This should veto and schedule a retry.
	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if !veto.Vetoed {
		t.Fatal("expected veto")
	}

	// Wait for the goroutine to fire the retry event.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	got := retryCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected 1 gate.llm.retry event, got %d", got)
	}
}

func TestRateLimiter_WindowExpiry_AllowsThrough(t *testing.T) {
	bus := engine.NewEventBus()

	// Use a real short window and real time.Now — just wait for it to expire.
	p := &Plugin{
		bus:               bus,
		logger:            slog.Default(),
		requestsPerWindow: 1,
		windowDuration:    50 * time.Millisecond,
		pauseMessage:      "paused",
		nowFunc:           time.Now,
	}

	p.unsubs = append(p.unsubs,
		bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8)),
	)
	bus.Subscribe("io.output", func(_ engine.Event[any]) {})

	// Fill window.
	bus.EmitVetoable("before:llm.request", &events.LLMRequest{})

	// Wait for window to expire.
	time.Sleep(60 * time.Millisecond)

	// After window expired — should pass.
	veto, _ := bus.EmitVetoable("before:llm.request", &events.LLMRequest{})
	if veto.Vetoed {
		t.Fatal("should not veto after window expiry")
	}
}
