package conversation

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

const pluginID = "nexus.memory.conversation"

// Plugin maintains a sliding window of conversation messages in memory.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	mu       sync.RWMutex
	messages []events.Message
	unsubs   []func()

	// internalCallIDs tracks ToolCall IDs marked ParentCallID!="" — i.e.
	// sub-calls fired from inside another tool (e.g. run_code scripts).
	// Their invoke/result pair is excluded from history so we never send
	// the LLM tool_use_ids it didn't generate.
	internalCallIDs map[string]struct{}

	maxMessages int
	persist     bool
}

// New creates a new conversation memory plugin.
func New() engine.Plugin {
	return &Plugin{
		maxMessages:     100,
		persist:         true,
		internalCallIDs: make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Conversation Memory" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 10},
		{EventType: "io.output", Priority: 10},
		{EventType: "tool.invoke", Priority: 10},
		{EventType: "tool.result", Priority: 10},
		{EventType: "memory.store", Priority: 50},
		{EventType: "memory.query", Priority: 50},
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

	// Read config.
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
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.store", p.handleStore, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.query", p.handleQuery, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compacted", p.handleCompacted, engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("conversation memory plugin initialized", "max_messages", p.maxMessages, "persist", p.persist)
	return nil
}

func (p *Plugin) Ready() error {
	// Preload conversation history if recalling a session with existing data.
	if p.session != nil && p.session.FileExists("context/conversation.jsonl") {
		if err := p.loadHistory(); err != nil {
			p.logger.Warn("failed to preload conversation history", "error", err)
		}
	}
	return nil
}

// loadHistory reads persisted conversation messages from the session workspace.
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

	// Apply sliding window.
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

// GetHistory returns the current message buffer.
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
	msg := events.Message{
		Role:    "user",
		Content: input.Content,
	}
	p.appendMessage(msg)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	msg := events.Message{
		Role:    out.Role,
		Content: out.Content,
	}
	p.appendMessage(msg)
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
		return
	}
	content, _ := json.Marshal(tc.Arguments)
	msg := events.Message{
		Role:       "tool_invoke",
		Content:    fmt.Sprintf("[%s] %s", tc.Name, string(content)),
		ToolCallID: tc.ID,
	}
	p.appendMessage(msg)
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
	msg := events.Message{
		Role:       "tool_result",
		Content:    fmt.Sprintf("[%s] %s", result.Name, content),
		ToolCallID: result.ID,
	}
	p.appendMessage(msg)
}

func (p *Plugin) handleStore(e engine.Event[any]) {
	entry, ok := e.Payload.(events.MemoryEntry)
	if !ok {
		return
	}
	msg := events.Message{
		Role:    entry.Key,
		Content: entry.Content,
	}
	p.appendMessage(msg)
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
			matched = append(matched, events.MemoryEntry{
				Key:       msg.Role,
				Content:   msg.Content,
				SessionID: query.SessionID,
			})
		}
	}
	p.mu.RUnlock()

	// Apply limit.
	if query.Limit > 0 && len(matched) > query.Limit {
		matched = matched[len(matched)-query.Limit:]
	}

	_ = p.bus.Emit("memory.result", events.MemoryResult{
		Entries: matched,
		Query:   query.Query,
	})
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

	// Re-persist the compacted conversation, replacing the existing file.
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
	}
	p.mu.Unlock()

	// Persist to session workspace if enabled.
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
