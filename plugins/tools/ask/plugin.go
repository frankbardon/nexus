package ask

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.ask"
	pluginName = "Ask User Tool"
	version    = "0.1.0"
)

// Plugin implements a tool that lets the agent ask the user a question
// and receive a free-form text response.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	unsubs []func()

	mu      sync.Mutex
	pending map[string]chan string // promptID -> response channel
}

// New creates a new ask user tool plugin.
func New() engine.Plugin {
	return &Plugin{
		pending: make(map[string]chan string),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.ask.response", p.handleAskResponse,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        "ask_user",
		Description: "Ask the user a question and wait for their response. Use this when you need clarification, confirmation, or additional information from the user before proceeding.",
		Class:       "communication",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask the user",
				},
			},
			"required": []string{"question"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "io.ask.response", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
		"tool.register",
		"io.ask",
	}
}

func (p *Plugin) handleToolInvoke(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != "ask_user" {
		return
	}

	question, _ := tc.Arguments["question"].(string)
	if question == "" {
		p.emitResult(tc, "", "question argument is required")
		return
	}

	promptID := fmt.Sprintf("ask-%s-%s", tc.TurnID, tc.ID)

	// Create a channel to wait for the user's response.
	ch := make(chan string, 1)
	p.mu.Lock()
	p.pending[promptID] = ch
	p.mu.Unlock()

	// Ask the user via the IO layer.
	_ = p.bus.Emit("io.ask", events.AskUser{
		PromptID: promptID,
		Question: question,
		TurnID:   tc.TurnID,
	})

	// Block until the user responds.
	answer := <-ch

	p.mu.Lock()
	delete(p.pending, promptID)
	p.mu.Unlock()

	p.emitResult(tc, answer, "")
}

func (p *Plugin) handleAskResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.AskUserResponse)
	if !ok {
		return
	}

	p.mu.Lock()
	ch, exists := p.pending[resp.PromptID]
	p.mu.Unlock()

	if exists {
		ch <- resp.Answer
	}
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{
		ID:     tc.ID,
		Name:   tc.Name,
		Output: output,
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
