package contextwindow

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.context_window"

const (
	defaultMaxContextTokens = 100000
	defaultTriggerRatio     = 0.85
	defaultCharsPerToken    = 4.0
)

// New creates a new context window gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		maxContextTokens: defaultMaxContextTokens,
		triggerRatio:     defaultTriggerRatio,
		charsPerToken:    defaultCharsPerToken,
	}
}

// Plugin gates before:llm.request events by estimating token count of outgoing
// messages. When approaching the provider limit, vetoes and triggers compaction.
// After compaction completes, emits gate.llm.retry so agents auto-resume.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	maxContextTokens int
	triggerRatio     float64
	charsPerToken    float64

	// pendingRetry tracks whether we vetoed a request and are waiting for compaction.
	mu           sync.Mutex
	pendingRetry bool

	unsubs []func()
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Context Window Gate" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["max_context_tokens"].(int); ok && v > 0 {
		p.maxContextTokens = v
	} else if v, ok := ctx.Config["max_context_tokens"].(float64); ok && v > 0 {
		p.maxContextTokens = int(v)
	}

	if v, ok := ctx.Config["trigger_ratio"].(float64); ok && v > 0 && v < 1 {
		p.triggerRatio = v
	}

	if v, ok := ctx.Config["chars_per_token"].(float64); ok && v > 0 {
		p.charsPerToken = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(15), engine.WithSource(pluginID)),
		// Subscribe to memory.compacted at priority 60 — after agents (50) update
		// their history — so we can emit gate.llm.retry with fresh state.
		p.bus.Subscribe("memory.compacted", p.handleMemoryCompacted,
			engine.WithPriority(60), engine.WithSource(pluginID)),
	)

	p.logger.Info("context window gate initialized",
		"max_context_tokens", p.maxContextTokens,
		"trigger_ratio", p.triggerRatio,
		"chars_per_token", p.charsPerToken)
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
		{EventType: "before:llm.request", Priority: 15},
		{EventType: "memory.compacted", Priority: 60},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"memory.compact.request", "io.output", "gate.llm.retry"}
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

	// Skip compaction plugin's own LLM requests.
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}

	// Estimate token count.
	estimated := p.estimateTokens(req.Messages)
	threshold := int(float64(p.maxContextTokens) * p.triggerRatio)

	if estimated >= threshold {
		p.logger.Warn("context window approaching limit, triggering compaction",
			"estimated_tokens", estimated,
			"threshold", threshold,
			"max", p.maxContextTokens)

		p.mu.Lock()
		p.pendingRetry = true
		p.mu.Unlock()

		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("Context window approaching limit (%d / %d estimated tokens), triggering compaction",
				estimated, p.maxContextTokens),
		}

		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: fmt.Sprintf("Context approaching limit (%d estimated tokens). Compacting conversation...", estimated),
			Role:    "system",
		})

		// Trigger compaction.
		_ = p.bus.Emit("memory.compact.request", map[string]any{
			"reason":           "context_window_gate",
			"estimated_tokens": estimated,
			"threshold":        threshold,
		})
	}
}

// handleMemoryCompacted fires after compaction completes and agents have updated
// their history. If we triggered the compaction, emit gate.llm.retry so the
// agent auto-resumes instead of stalling.
func (p *Plugin) handleMemoryCompacted(_ engine.Event[any]) {
	p.mu.Lock()
	pending := p.pendingRetry
	p.pendingRetry = false
	p.mu.Unlock()

	if !pending {
		return
	}

	p.logger.Info("compaction complete, emitting gate.llm.retry")
	_ = p.bus.Emit("gate.llm.retry", map[string]any{
		"source": pluginID,
		"reason": "compaction_complete",
	})
}

// estimateTokens estimates the token count of a message list using char/token ratio.
func (p *Plugin) estimateTokens(messages []events.Message) int {
	totalChars := 0
	for _, msg := range messages {
		totalChars += utf8.RuneCountInString(msg.Content)
	}
	return int(float64(totalChars) / p.charsPerToken)
}
