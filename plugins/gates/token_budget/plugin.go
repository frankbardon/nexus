package tokenbudget

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.token_budget"

const (
	defaultMaxTokens        = 100000
	defaultWarningThreshold = 0.8
)

// New creates a new token budget gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		maxTokens:        defaultMaxTokens,
		warningThreshold: defaultWarningThreshold,
		message:          "Token budget exhausted for this session.",
	}
}

// Plugin gates before:llm.request events by checking cumulative session token usage.
// Vetoes when usage exceeds the configured ceiling.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	maxTokens        int
	warningThreshold float64
	message          string
	warningEmitted   bool

	unsubs []func()
}

func (p *Plugin) ID() string           { return pluginID }
func (p *Plugin) Name() string         { return "Token Budget Gate" }
func (p *Plugin) Version() string      { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	if v, ok := ctx.Config["max_tokens"].(int); ok && v > 0 {
		p.maxTokens = v
	} else if v, ok := ctx.Config["max_tokens"].(float64); ok && v > 0 {
		p.maxTokens = int(v)
	}

	if v, ok := ctx.Config["warning_threshold"].(float64); ok && v > 0 && v < 1 {
		p.warningThreshold = v
	}

	if v, ok := ctx.Config["message"].(string); ok && v != "" {
		p.message = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	p.logger.Info("token budget gate initialized",
		"max_tokens", p.maxTokens,
		"warning_threshold", p.warningThreshold)
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
	return []string{"io.output"}
}

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}

	if p.session == nil {
		return
	}

	meta, err := p.session.SessionMetadata()
	if err != nil {
		p.logger.Error("failed to read session metadata", "error", err)
		return
	}

	used := meta.TokensUsed
	threshold := int(float64(p.maxTokens) * p.warningThreshold)

	// Emit warning once when approaching limit.
	if !p.warningEmitted && used >= threshold && used < p.maxTokens {
		p.warningEmitted = true
		pct := float64(used) / float64(p.maxTokens) * 100
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: fmt.Sprintf("Warning: %.0f%% of token budget used (%d / %d tokens).", pct, used, p.maxTokens),
			Role:    "system",
		})
	}

	// Veto if over budget.
	if used >= p.maxTokens {
		p.logger.Warn("token budget exceeded",
			"used", used, "max", p.maxTokens)
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Token budget exhausted (%d / %d)", used, p.maxTokens),
		}
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: p.message,
			Role:    "system",
		})
	}
}
