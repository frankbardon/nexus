package catalog

import (
	"context"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID = "nexus.tool.catalog"
	version  = "0.1.0"
)

// Plugin maintains a live registry of tool definitions emitted on
// "tool.register" and answers synchronous "tool.catalog.query" requests so
// agent plugins don't each need to cache the same list. Registration order
// is preserved; last-write-wins when a tool with the same Name re-registers.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	mu     sync.RWMutex
	tools  []events.ToolDef
	unsubs []func()
}

// New creates a new tool catalog plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return "Tool Catalog" }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.register", p.handleRegister,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.catalog.query", p.handleCatalogQuery,
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
		{EventType: "tool.register", Priority: 10},
		{EventType: "tool.catalog.query", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string { return nil }

// handleRegister records a new tool definition. If the same Name is
// re-registered (rare, but possible during plugin reload), the existing
// entry is replaced so duplicates don't accumulate.
func (p *Plugin) handleRegister(e engine.Event[any]) {
	td, ok := e.Payload.(events.ToolDef)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.tools {
		if existing.Name == td.Name {
			p.tools[i] = td
			return
		}
	}
	p.tools = append(p.tools, td)
	p.logger.Info("tool registered", "name", td.Name, "class", td.Class)
}

// handleCatalogQuery fills the pointer payload with a snapshot of the tool
// list. Caller reads q.Tools after Emit returns.
func (p *Plugin) handleCatalogQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.ToolCatalogQuery)
	if !ok {
		return
	}
	p.mu.RLock()
	out := make([]events.ToolDef, len(p.tools))
	copy(out, p.tools)
	p.mu.RUnlock()
	q.Tools = out
}
