package endlessloop

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.endless_loop"

const (
	defaultMaxIterations = 25
)

// New creates a new endless loop gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		maxIterations: defaultMaxIterations,
	}
}

// Plugin gates before:llm.request events by counting internal LLM calls per turn.
// Vetoes when the count exceeds the configured limit.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	maxIterations int
	warningAt     int

	mu             sync.Mutex
	iterationCount int
	warned         bool
	unsubs         []func()
}

func (p *Plugin) ID() string           { return pluginID }
func (p *Plugin) Name() string         { return "Endless Loop Gate" }
func (p *Plugin) Version() string      { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["max_iterations"].(int); ok && v > 0 {
		p.maxIterations = v
	} else if v, ok := ctx.Config["max_iterations"].(float64); ok && v > 0 {
		p.maxIterations = int(v)
	}

	if v, ok := ctx.Config["warning_at"].(int); ok && v > 0 {
		p.warningAt = v
	} else if v, ok := ctx.Config["warning_at"].(float64); ok && v > 0 {
		p.warningAt = int(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart,
			engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.input", p.handleInput,
			engine.WithPriority(5), engine.WithSource(pluginID)),
	)

	p.logger.Info("endless loop gate initialized",
		"max_iterations", p.maxIterations,
		"warning_at", p.warningAt)
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
		{EventType: "before:llm.request", Priority: 10},
		{EventType: "agent.turn.start", Priority: 5},
		{EventType: "io.input", Priority: 5},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"io.output"}
}

func (p *Plugin) handleTurnStart(_ engine.Event[any]) {
	p.mu.Lock()
	p.iterationCount = 0
	p.warned = false
	p.mu.Unlock()
}

func (p *Plugin) handleInput(_ engine.Event[any]) {
	p.mu.Lock()
	p.iterationCount = 0
	p.warned = false
	p.mu.Unlock()
}

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}

	// Skip gate-originated or planner LLM requests.
	if req, ok := vp.Original.(*events.LLMRequest); ok {
		if src, _ := req.Metadata["_source"].(string); src != "" {
			return
		}
	}

	p.mu.Lock()
	p.iterationCount++
	count := p.iterationCount
	warned := p.warned
	p.mu.Unlock()

	if p.warningAt > 0 && count == p.warningAt && !warned {
		p.mu.Lock()
		p.warned = true
		p.mu.Unlock()
		remaining := p.maxIterations - count
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: fmt.Sprintf("Warning: %d iterations used, %d remaining before limit.", count, remaining),
			Role:    "system",
		})
	}

	if count > p.maxIterations {
		p.logger.Warn("endless loop gate triggered",
			"iteration", count, "max", p.maxIterations)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Maximum iterations reached (%d)", p.maxIterations),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: fmt.Sprintf("I've reached the maximum number of iterations (%d). Stopping to prevent an endless loop.", p.maxIterations),
			Role:    "system",
		})
	}
}
