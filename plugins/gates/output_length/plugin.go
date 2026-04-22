package outputlength

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/gates/internal/retry"
)

const pluginID = "nexus.gate.output_length"

const defaultRetryPrompt = `Your response was {length} characters, exceeding the {limit} character limit. Please provide a more concise response that covers the same key points in fewer characters.`

// New creates a new output length gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		maxChars:    5000,
		maxRetries:  2,
		retryPrompt: defaultRetryPrompt,
	}
}

// Plugin gates before:io.output events by checking character count.
// On exceed, retries via LLM asking for concise version.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	maxChars    int
	maxRetries  int
	retryPrompt string

	retrier *retry.Handler
	unsubs  []func()
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Output Length Gate" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["max_chars"].(int); ok && v > 0 {
		p.maxChars = v
	} else if v, ok := ctx.Config["max_chars"].(float64); ok && v > 0 {
		p.maxChars = int(v)
	}

	if v, ok := ctx.Config["max_retries"].(int); ok && v >= 0 {
		p.maxRetries = v
	} else if v, ok := ctx.Config["max_retries"].(float64); ok && v >= 0 {
		p.maxRetries = int(v)
	}

	if v, ok := ctx.Config["retry_prompt"].(string); ok && v != "" {
		p.retryPrompt = v
	}

	p.retrier = retry.New(p.bus, p.logger, retry.Config{
		MaxRetries:  p.maxRetries,
		RetryPrompt: p.retryPrompt,
		Source:      pluginID,
	})

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:io.output", p.handleBeforeOutput,
			engine.WithPriority(12), engine.WithSource(pluginID)),
	)

	p.logger.Info("output length gate initialized",
		"max_chars", p.maxChars,
		"max_retries", p.maxRetries)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	p.retrier.Shutdown()
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:io.output", Priority: 12},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"llm.request", "io.output"}
}

func (p *Plugin) handleBeforeOutput(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	output, ok := vp.Original.(*events.AgentOutput)
	if !ok {
		return
	}

	if output.Role != "assistant" {
		return
	}

	limit := fmt.Sprintf("%d", p.maxChars)
	result := p.retrier.AttemptRetry(
		output.Content,
		nil,
		func(content string) string {
			if len(content) > p.maxChars {
				return fmt.Sprintf("response is %d characters (limit: %d)", len(content), p.maxChars)
			}
			return ""
		},
		map[string]string{
			"length": fmt.Sprintf("%d", len(output.Content)),
			"limit":  limit,
		},
	)

	if result.Valid {
		output.Content = result.Content
		return
	}

	// Retries exhausted — allow through but warn.
	p.logger.Warn("output length exceeded after retries",
		"length", len(result.Content), "max", p.maxChars)
	// Don't veto — let the long response through with a warning.
	_ = p.bus.Emit("io.output", events.AgentOutput{
		Content: fmt.Sprintf("Note: response exceeds %d character limit (%d chars) after %d retry attempts.",
			p.maxChars, len(result.Content), p.maxRetries),
		Role: "system",
	})
}
