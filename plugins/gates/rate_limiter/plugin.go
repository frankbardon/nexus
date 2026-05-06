// Package ratelimiter implements nexus.gate.rate_limiter, the LLM-request
// rate gate. Two modes:
//
//   - mode: reject (default) — vetoes before:llm.request when the window
//     budget is exhausted; emits gate.llm.retry from a background goroutine
//     once the oldest timestamp ages out, letting the agent retry.
//   - mode: queue — vetoes the request and queues a retry slot up to
//     queue.max_pending; a single drainer goroutine emits gate.llm.retry
//     events at the configured rate. Excess (queue full) is rejected.
package ratelimiter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.rate_limiter"

const (
	defaultRequestsPerMinute = 60
	defaultWindowSeconds     = 60
	defaultMaxPending        = 100

	ModeReject = "reject"
	ModeQueue  = "queue"
)

// New creates a new rate limiter gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		mode:              ModeReject,
		requestsPerWindow: defaultRequestsPerMinute,
		windowDuration:    time.Duration(defaultWindowSeconds) * time.Second,
		maxPending:        defaultMaxPending,
		pauseMessage:      "Rate limit reached. Pausing for {seconds}s...",
	}
}

// Plugin gates before:llm.request events by tracking request rate. When the
// rate budget is exhausted it vetoes the request and (depending on mode)
// either schedules a single one-shot retry (reject) or pumps a bounded
// queue at the allowed rate (queue).
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	mode              string
	requestsPerWindow int
	windowDuration    time.Duration
	maxPending        int
	pauseMessage      string

	// nowFunc for testing — defaults to time.Now.
	nowFunc func() time.Time

	mu         sync.Mutex
	timestamps []time.Time

	// queueCh and drainerDone exist only when mode == queue. The drainer
	// goroutine reads from queueCh; closing it (in Shutdown) stops the
	// drainer.
	queueCh     chan struct{}
	drainerDone chan struct{}
	stopCh      chan struct{}

	unsubs []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Rate Limiter Gate" }
func (p *Plugin) Version() string                   { return "0.2.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.nowFunc = time.Now

	if v, ok := ctx.Config["mode"].(string); ok && v != "" {
		p.mode = v
	}
	if p.mode != ModeReject && p.mode != ModeQueue {
		return fmt.Errorf("mode %q: must be %q or %q", p.mode, ModeReject, ModeQueue)
	}

	if v, ok := ctx.Config["requests_per_minute"].(int); ok && v > 0 {
		p.requestsPerWindow = v
	} else if v, ok := ctx.Config["requests_per_minute"].(float64); ok && v > 0 {
		p.requestsPerWindow = int(v)
	}

	if v, ok := ctx.Config["window_seconds"].(int); ok && v > 0 {
		p.windowDuration = time.Duration(v) * time.Second
	} else if v, ok := ctx.Config["window_seconds"].(float64); ok && v > 0 {
		p.windowDuration = time.Duration(int(v)) * time.Second
	}

	if v, ok := ctx.Config["pause_message"].(string); ok && v != "" {
		p.pauseMessage = v
	}

	if q, ok := ctx.Config["queue"].(map[string]any); ok {
		if v, ok := q["max_pending"].(int); ok && v > 0 {
			p.maxPending = v
		} else if v, ok := q["max_pending"].(float64); ok && v > 0 {
			p.maxPending = int(v)
		}
	}

	if p.mode == ModeQueue {
		p.queueCh = make(chan struct{}, p.maxPending)
		p.drainerDone = make(chan struct{})
		p.stopCh = make(chan struct{})
		go p.drain()
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8), engine.WithSource(pluginID)),
	)

	p.logger.Info("rate limiter gate initialized",
		"mode", p.mode,
		"requests_per_window", p.requestsPerWindow,
		"window_seconds", p.windowDuration.Seconds(),
		"max_pending", p.maxPending)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.mode == ModeQueue && p.stopCh != nil {
		close(p.stopCh)
		// Wait for drainer to exit so tests don't see leaked goroutines.
		<-p.drainerDone
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 8},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"io.output", "gate.llm.retry"}
}

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}

	now := p.nowFunc()
	cutoff := now.Add(-p.windowDuration)

	p.mu.Lock()
	// Prune timestamps outside the window.
	valid := 0
	for _, ts := range p.timestamps {
		if ts.After(cutoff) {
			p.timestamps[valid] = ts
			valid++
		}
	}
	p.timestamps = p.timestamps[:valid]

	if len(p.timestamps) < p.requestsPerWindow {
		// Under limit — record and proceed.
		p.timestamps = append(p.timestamps, now)
		p.mu.Unlock()
		return
	}

	// Window full — calculate wait time.
	oldest := p.timestamps[0]
	waitUntil := oldest.Add(p.windowDuration)
	waitDuration := waitUntil.Sub(now)
	if waitDuration <= 0 {
		// Window just expired — record and proceed.
		p.timestamps = append(p.timestamps, now)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	switch p.mode {
	case ModeQueue:
		p.handleQueueMode(vp, waitDuration)
	default:
		p.handleRejectMode(vp, waitDuration)
	}
}

func (p *Plugin) handleRejectMode(vp *engine.VetoablePayload, waitDuration time.Duration) {
	seconds := int(waitDuration.Seconds()) + 1
	msg := strings.ReplaceAll(p.pauseMessage, "{seconds}", fmt.Sprintf("%d", seconds))

	p.logger.Info("rate limit reached (reject mode), scheduling retry",
		"wait_seconds", seconds)

	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("Rate limited: retry in %ds", seconds),
	}

	_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: msg,
		Role: "system",
	})

	// Non-blocking one-shot retry after the wait period.
	go func(d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		<-t.C

		p.mu.Lock()
		p.timestamps = append(p.timestamps, p.nowFunc())
		p.mu.Unlock()

		p.logger.Info("rate limit wait complete, emitting gate.llm.retry")
		_ = p.bus.Emit("gate.llm.retry", map[string]any{
			"source": pluginID,
			"reason": "rate_limit_window_reset",
		})
	}(waitDuration)
}

func (p *Plugin) handleQueueMode(vp *engine.VetoablePayload, waitDuration time.Duration) {
	// Try to enqueue without blocking. Capacity full = reject outright.
	select {
	case p.queueCh <- struct{}{}:
	default:
		p.logger.Warn("rate limit queue full, rejecting request",
			"max_pending", p.maxPending)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Rate limited: queue full (max_pending=%d)", p.maxPending),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: fmt.Sprintf("Rate limit queue full (%d pending). Request rejected.", p.maxPending),
			Role: "system",
		})
		return
	}

	seconds := int(waitDuration.Seconds()) + 1
	msg := strings.ReplaceAll(p.pauseMessage, "{seconds}", fmt.Sprintf("%d", seconds))

	p.logger.Info("rate limit reached (queue mode), enqueued retry",
		"queue_depth", len(p.queueCh))

	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("Rate limited (queued): retry in ~%ds", seconds),
	}

	_ = p.bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: msg,
		Role: "system",
	})
}

// drain pumps queued retry slots out at the configured rate. One slot per
// (windowDuration / requestsPerWindow) interval. Exits when stopCh closes.
func (p *Plugin) drain() {
	defer close(p.drainerDone)

	interval := p.windowDuration / time.Duration(p.requestsPerWindow)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
		}

		select {
		case <-p.stopCh:
			return
		case <-p.queueCh:
			// Reserve a slot in the rate window so the upcoming request
			// counts against the budget.
			p.mu.Lock()
			p.timestamps = append(p.timestamps, p.nowFunc())
			p.mu.Unlock()

			p.logger.Info("draining rate-limit queue slot",
				"queue_depth", len(p.queueCh))
			_ = p.bus.Emit("gate.llm.retry", map[string]any{
				"source": pluginID,
				"reason": "rate_limit_queue_drain",
			})
		default:
			// Empty queue this tick.
		}
	}
}
