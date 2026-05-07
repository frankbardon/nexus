package capped

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.memory.capped"

// Plugin maintains a sliding window of LLM-native conversation messages for
// the active session and exposes them to other plugins (agents, compaction)
// via a synchronous bus query. Storage format mirrors events.Message exactly
// — roles are "user", "assistant" (with ToolCalls populated when the model
// requested tools), and "tool" (with ToolCallID) — so consumers can feed
// the buffer directly into an LLMRequest without translation.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	mu       sync.RWMutex
	messages []events.Message
	unsubs   []func()

	// internalCallIDs tracks ToolCall IDs marked ParentCallID!="" — i.e.
	// sub-calls fired from inside another tool (e.g. run_code scripts).
	// Their result is excluded from history so the LLM never sees
	// tool_use_ids it didn't generate.
	internalCallIDs map[string]struct{}

	maxMessages int
	persist     bool
}

// New creates a new capped conversation memory plugin.
func New() engine.Plugin {
	return &Plugin{
		maxMessages:     100,
		persist:         true,
		internalCallIDs: make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return "Capped Conversation Memory" }
func (p *Plugin) Version() string                { return "0.2.0" }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as the default provider of
// "memory.history" — the LLM-native conversation buffer consumed via
// "memory.history.query" by agents and compactors.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "memory.history",
			Description: "LLM-native conversation history (user/assistant/tool messages) for the active session.",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 10},
		{EventType: "llm.response", Priority: 10},
		{EventType: "tool.invoke", Priority: 10},
		{EventType: "tool.result", Priority: 10},
		{EventType: "memory.store", Priority: 50},
		{EventType: "memory.query", Priority: 50},
		{EventType: "memory.history.query", Priority: 50},
		{EventType: "memory.compacted", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"memory.result"}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	if v, ok := ctx.Config["max_messages"]; ok {
		if n, ok := v.(int); ok && n > 0 {
			p.maxMessages = n
		}
	}
	if v, ok := ctx.Config["persist"]; ok {
		if b, ok := v.(bool); ok {
			p.persist = b
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInput, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.store", p.handleStore, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.query", p.handleQuery, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.history.query", p.handleHistoryQuery, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compacted", p.handleCompacted, engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("conversation memory plugin initialized", "max_messages", p.maxMessages, "persist", p.persist)
	return nil
}

func (p *Plugin) Ready() error {
	if p.session != nil && p.session.FileExists("context/conversation.jsonl") {
		if err := p.loadHistory(); err != nil {
			p.logger.Warn("failed to preload conversation history", "error", err)
		}
	}
	return nil
}

func (p *Plugin) loadHistory() error {
	data, err := p.session.ReadFile("context/conversation.jsonl")
	if err != nil {
		return fmt.Errorf("reading conversation history: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg events.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			p.logger.Warn("skipping malformed conversation entry", "error", err)
			continue
		}
		p.messages = append(p.messages, msg)
	}

	if len(p.messages) > p.maxMessages {
		p.messages = p.messages[len(p.messages)-p.maxMessages:]
	}

	p.logger.Info("preloaded conversation history", "messages", len(p.messages))
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// GetHistory returns a copy of the current message buffer. Retained for
// in-process callers (tests, embedders) — event-driven consumers should use
// the "memory.history.query" bus event instead.
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
	p.appendMessage(events.Message{
		Role:    "user",
		Content: input.Content,
	})
}

// handleLLMResponse records the model's assistant message in native form,
// preserving any ToolCalls so the next request carries a matched tool_use →
// tool_result pair structure. Skips responses tagged for other plugins (e.g.
// planner-internal calls that use Metadata["_source"]).
//
// Provider-specific round-trip data (e.g. Anthropic extended-thinking blocks
// with cryptographic signatures) is forwarded via Message.Metadata so the
// next request can echo them back verbatim. Anthropic returns HTTP 400 if
// thinking_blocks aren't preserved on the assistant turn that follows a
// tool_result.
func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	if source, _ := resp.Metadata["_source"].(string); source != "" {
		return
	}
	p.appendMessage(events.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
		Metadata:  forwardMessageMetadata(resp.Metadata),
	})
}

// forwardMessageMetadata extracts the subset of LLMResponse.Metadata that
// should travel with a stored Message. We deliberately do NOT copy the whole
// map — engine-internal flags like _source / _structured_output / _target_*
// are request-scoped routing hints, not message-scoped state. Only keys that
// providers identify as required for round-trip continuity are preserved.
func forwardMessageMetadata(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	var out map[string]any
	if v, ok := src["thinking_blocks"]; ok {
		out = map[string]any{"thinking_blocks": v}
	}
	return out
}

// handleToolInvoke only tracks the internal-call filter; the invocation
// itself is already represented on the prior assistant message's ToolCalls
// field, so we don't append anything here.
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
	p.appendMessage(events.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: result.ID,
	})
}

func (p *Plugin) handleStore(e engine.Event[any]) {
	entry, ok := e.Payload.(events.MemoryEntry)
	if !ok {
		return
	}
	p.appendMessage(events.Message{
		Role:    entry.Key,
		Content: entry.Content,
	})
}

func (p *Plugin) handleQuery(e engine.Event[any]) {
	query, ok := e.Payload.(events.MemoryQuery)
	if !ok {
		return
	}

	p.mu.RLock()
	var matched []events.MemoryEntry
	for _, msg := range p.messages {
		if query.Query == "" || strings.Contains(strings.ToLower(msg.Content), strings.ToLower(query.Query)) {
			matched = append(matched, events.MemoryEntry{SchemaVersion: events.MemoryEntryVersion, Key: msg.Role,
				Content:   msg.Content,
				SessionID: query.SessionID,
			})
		}
	}
	p.mu.RUnlock()

	if query.Limit > 0 && len(matched) > query.Limit {
		matched = matched[len(matched)-query.Limit:]
	}

	_ = p.bus.Emit("memory.result", events.MemoryResult{SchemaVersion: events.MemoryResultVersion, Entries: matched,
		Query: query.Query,
	})
}

// handleHistoryQuery satisfies synchronous requests for the LLM-native message
// list. Handler mutates the HistoryQuery pointer in place; caller reads
// q.Messages after Emit returns (same pattern as VetoablePayload).
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

	if p.persist && p.session != nil {
		var buf []byte
		for _, msg := range cc.Messages {
			data, err := json.Marshal(msg)
			if err != nil {
				p.logger.Error("failed to marshal compacted message", "error", err)
				continue
			}
			buf = append(buf, data...)
			buf = append(buf, '\n')
		}
		if err := p.session.WriteFile("context/conversation.jsonl", buf); err != nil {
			p.logger.Error("failed to persist compacted conversation", "error", err)
		}
	}

	p.logger.Info("conversation replaced by compaction", "messages", len(cc.Messages))
}

func (p *Plugin) appendMessage(msg events.Message) {
	p.mu.Lock()
	p.messages = append(p.messages, msg)
	if len(p.messages) > p.maxMessages {
		p.messages = p.messages[len(p.messages)-p.maxMessages:]
		// Pair-safe adjustment: if the cap landed us with leading "tool"
		// messages, their matching assistant tool_use was in the dropped
		// prefix. The LLM provider would reject tool results whose
		// tool_use_id has no preceding declaration, so drop those orphans
		// too. This undershoots the cap briefly but keeps the buffer
		// well-formed for the next llm.request.
		for len(p.messages) > 0 && p.messages[0].Role == "tool" {
			p.messages = p.messages[1:]
		}
	}
	p.mu.Unlock()

	if p.persist && p.session != nil {
		data, err := json.Marshal(msg)
		if err != nil {
			p.logger.Error("failed to marshal message for persistence", "error", err)
			return
		}
		data = append(data, '\n')
		if err := p.session.AppendFile("context/conversation.jsonl", data); err != nil {
			p.logger.Error("failed to persist conversation message", "error", err)
		}
	}
}
