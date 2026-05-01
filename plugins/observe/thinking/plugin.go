package thinking

import (
	"context"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
)

const (
	pluginID   = "nexus.observe.thinking"
	pluginName = "Thinking Observer"
	version    = "0.2.0"
)

// Plugin is a marker observer for thinking.step and plan.progress events.
// The journal is the single source of truth for these events — every
// envelope on the bus lands in the per-session journal, so anything that
// needs the thinking history reads it from there (live via
// journal.Writer.SubscribeProjection, or post-mortem via
// journal.ProjectFile).
//
// This plugin no longer writes derived JSONL files; its presence in
// plugins.active acts as a UI feature flag that TUI / browser shells use
// to enable thinking-related UI affordances. The Subscriptions() entries
// are retained so the plugin shows up in registry / manifest tooling that
// inventories which event types each plugin attends to.
type Plugin struct {
	logger *slog.Logger
}

// New creates a new thinking observer plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.logger.Info("thinking observer initialized — journal is source of truth for thinking.step + plan.progress")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "thinking.step", Priority: 90},
		{EventType: "plan.progress", Priority: 90},
	}
}

func (p *Plugin) Emissions() []string { return nil }
