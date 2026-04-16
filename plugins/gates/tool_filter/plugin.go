package toolfilter

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.tool_filter"

// New creates a new tool filter gate plugin instance.
func New() engine.Plugin {
	return &Plugin{}
}

// Plugin gates before:llm.request events by filtering the available tool set.
// Supports include (allowlist) or exclude (blocklist) modes via config.
// Include takes precedence if both are specified.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	include []string
	exclude []string

	unsubs []func()
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Tool Filter Gate" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["include"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				p.include = append(p.include, s)
			}
		}
	}

	if v, ok := ctx.Config["exclude"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				p.exclude = append(p.exclude, s)
			}
		}
	}

	if len(p.include) > 0 && len(p.exclude) > 0 {
		p.logger.Warn("both include and exclude configured; include takes precedence")
		p.exclude = nil
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	mode := "passthrough"
	if len(p.include) > 0 {
		mode = fmt.Sprintf("include(%d tools)", len(p.include))
	} else if len(p.exclude) > 0 {
		mode = fmt.Sprintf("exclude(%d tools)", len(p.exclude))
	}
	p.logger.Info("tool filter gate initialized", "mode", mode)
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
	}
}

func (p *Plugin) Emissions() []string {
	return nil
}

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}

	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip gate-originated or planner LLM requests.
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}

	// Merge gate config into the request's ToolFilter.
	// Request-level filter (set by agent or other plugin) takes precedence.
	if req.ToolFilter != nil {
		return
	}

	if len(p.include) > 0 {
		req.ToolFilter = &events.ToolFilter{Include: p.include}
	} else if len(p.exclude) > 0 {
		req.ToolFilter = &events.ToolFilter{Exclude: p.exclude}
	}
}
