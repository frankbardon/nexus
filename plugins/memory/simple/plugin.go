// Package simple provides a minimal reference implementation of
// memory.history: an in-memory, append-only buffer with no cap and no
// persistence. Useful for tests, demos, and short-lived sessions where the
// complexity of capped/summary_buffer isn't warranted.
package simple

import (
	"context"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.memory.simple"

// Plugin holds an unbounded slice of LLM-native messages and serves the
// memory.history.query synchronous query used by agents and compactors.
// Behaviour intentionally tracks the capped plugin minus the bounds + disk
// writes so the two remain swappable.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	mu       sync.RWMutex
	messages []events.Message
	unsubs   []func()

	// internalCallIDs excludes tool_use_ids the LLM never generated (e.g.
	// run_code script sub-calls) from history. Same invariant as capped.
	internalCallIDs map[string]struct{}
}

// New creates a new simple memory plugin.
func New() engine.Plugin {
	return &Plugin{
		internalCallIDs: make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return "Simple Conversation Memory" }
func (p *Plugin) Version() string                { return "0.1.0" }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as a provider of "memory.history".
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "memory.history",
			Description: "Unbounded, in-memory LLM-native conversation history for the active session.",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 10},
		{EventType: "llm.response", Priority: 10},
		{EventType: "tool.invoke", Priority: 10},
		{EventType: "tool.result", Priority: 10},
		{EventType: "memory.history.query", Priority: 50},
		{EventType: "memory.compacted", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.history.query", p.handleHistoryQuery, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compacted", p.handleCompacted, engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("simple memory plugin initialized")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// GetHistory returns a copy of the buffer for in-process callers (tests).
func (p *Plugin) GetHistory() []events.Message {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]events.Message, len(p.messages))
	copy(out, p.messages)
	return out
}

func (p *Plugin) handleInput(e engine.Event[any]) {
	input, ok := e.Payload.(events.UserInput)
	if !ok {
		return
	}
	p.append(events.Message{Role: "user", Content: input.Content})
}

func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	if source, _ := resp.Metadata["_source"].(string); source != "" {
		return
	}
	p.append(events.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})
}

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.ParentCallID != "" {
		p.mu.Lock()
		p.internalCallIDs[tc.ID] = struct{}{}
		p.mu.Unlock()
	}
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	result, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	p.mu.Lock()
	if _, internal := p.internalCallIDs[result.ID]; internal {
		delete(p.internalCallIDs, result.ID)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	content := result.Output
	if result.Error != "" {
		content = "Error: " + result.Error
	}
	p.append(events.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: result.ID,
	})
}

func (p *Plugin) handleHistoryQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.HistoryQuery)
	if !ok {
		return
	}
	p.mu.RLock()
	out := make([]events.Message, len(p.messages))
	copy(out, p.messages)
	p.mu.RUnlock()
	q.Messages = out
}

func (p *Plugin) handleCompacted(e engine.Event[any]) {
	cc, ok := e.Payload.(events.CompactionComplete)
	if !ok {
		return
	}
	p.mu.Lock()
	p.messages = make([]events.Message, len(cc.Messages))
	copy(p.messages, cc.Messages)
	p.mu.Unlock()
}

func (p *Plugin) append(msg events.Message) {
	p.mu.Lock()
	p.messages = append(p.messages, msg)
	p.mu.Unlock()
}
