package cancel

import (
	"context"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.control.cancel"
	pluginName = "Cancellation Controller"
	version    = "0.1.0"
)

// Plugin coordinates cancellation of in-flight LLM calls and agent loops,
// with support for resuming accidentally cancelled operations.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	unsubs []func()

	mu            sync.Mutex
	activeTurnID  string // turn currently in progress
	cancelledTurn string // last cancelled turn (for resume)
	cancelling    bool   // cancellation in progress
}

// New creates a new cancellation controller plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.request", p.handleCancelRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.resume", p.handleResumeRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)

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
		{EventType: "agent.turn.start", Priority: 10},
		{EventType: "agent.turn.end", Priority: 10},
		{EventType: "cancel.request", Priority: 10},
		{EventType: "cancel.resume", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"cancel.active",
		"io.status",
	}
}

func (p *Plugin) handleTurnStart(event engine.Event[any]) {
	info, ok := event.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	p.mu.Lock()
	p.activeTurnID = info.TurnID
	p.cancelling = false
	p.mu.Unlock()
}

func (p *Plugin) handleTurnEnd(event engine.Event[any]) {
	p.mu.Lock()
	p.activeTurnID = ""
	p.cancelling = false
	p.mu.Unlock()
}

func (p *Plugin) handleCancelRequest(event engine.Event[any]) {
	_, ok := event.Payload.(events.CancelRequest)
	if !ok {
		return
	}

	p.mu.Lock()
	if p.activeTurnID == "" || p.cancelling {
		p.mu.Unlock()
		return
	}
	turnID := p.activeTurnID
	p.cancelling = true
	p.cancelledTurn = turnID
	p.mu.Unlock()

	p.logger.Info("cancelling turn", "turn_id", turnID)

	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  "cancelling",
		Detail: "Cancelling current operation...",
	})

	_ = p.bus.Emit("cancel.active", events.CancelActive{
		TurnID: turnID,
	})
}

func (p *Plugin) handleResumeRequest(event engine.Event[any]) {
	_, ok := event.Payload.(events.CancelResume)
	if !ok {
		return
	}

	p.mu.Lock()
	turnID := p.cancelledTurn
	if turnID == "" {
		p.mu.Unlock()
		p.logger.Debug("no cancelled turn to resume")
		return
	}
	p.cancelledTurn = ""
	p.mu.Unlock()

	p.logger.Info("resuming cancelled turn", "turn_id", turnID)
}
