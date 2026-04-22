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
)

// New creates a new rate limiter gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		requestsPerWindow: defaultRequestsPerMinute,
		windowDuration:    time.Duration(defaultWindowSeconds) * time.Second,
		pauseMessage:      "Rate limit reached. Pausing for {seconds}s...",
	}
}

// Plugin gates before:llm.request events by tracking request rate.
// When the rate limit is exceeded, vetoes the request and schedules a
// gate.llm.retry event after the window resets. Does not block the bus.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	requestsPerWindow int
	windowDuration    time.Duration
	pauseMessage      string

	// nowFunc for testing — defaults to time.Now.
	nowFunc func() time.Time

	mu         sync.Mutex
	timestamps []time.Time
	unsubs     []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Rate Limiter Gate" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.nowFunc = time.Now

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

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8), engine.WithSource(pluginID)),
	)

	p.logger.Info("rate limiter gate initialized",
		"requests_per_window", p.requestsPerWindow,
		"window_seconds", p.windowDuration.Seconds())
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
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

	// Veto the request — don't block the bus.
	seconds := int(waitDuration.Seconds()) + 1
	msg := strings.ReplaceAll(p.pauseMessage, "{seconds}", fmt.Sprintf("%d", seconds))

	p.logger.Info("rate limit reached, scheduling retry",
		"wait_seconds", seconds)

	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("Rate limited: retry in %ds", seconds),
	}

	_ = p.bus.Emit("io.output", events.AgentOutput{
		Content: msg,
		Role:    "system",
	})

	// Schedule non-blocking retry after the wait period.
	go func() {
		time.Sleep(waitDuration)

		p.mu.Lock()
		p.timestamps = append(p.timestamps, p.nowFunc())
		p.mu.Unlock()

		p.logger.Info("rate limit wait complete, emitting gate.llm.retry")
		_ = p.bus.Emit("gate.llm.retry", map[string]any{
			"source": pluginID,
			"reason": "rate_limit_window_reset",
		})
	}()
}
