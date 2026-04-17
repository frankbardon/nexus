package browser

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/ui"
)

const pluginID = "nexus.io.browser"

// Plugin is the browser IO plugin that serves a web UI over HTTP/WebSocket.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	system  *engine.SystemInfo
	adapter *Adapter
	server  *Server
	hub     *Hub
	unsubs  []func()

	host        string
	port        int
	openBrowser bool
}

// New creates a new browser IO plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Browser IO" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "io.output.stream", Priority: 50},
		{EventType: "io.output.stream.end", Priority: 50},
		{EventType: "io.output.clear", Priority: 50},
		{EventType: "io.status", Priority: 50},
		{EventType: "io.approval.request", Priority: 50},
		{EventType: "io.ask", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		{EventType: "plan.approval.request", Priority: 50},
		{EventType: "plan.created", Priority: 50},
		{EventType: "agent.plan", Priority: 50},
		{EventType: "provider.fallback", Priority: 50},
		{EventType: "session.file.created", Priority: 50},
		{EventType: "session.file.updated", Priority: 50},
		{EventType: "io.history.replay", Priority: 50},
		{EventType: "cancel.complete", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"io.approval.response",
		"io.ask.response",
		"plan.approval.response",
		"io.session.start",
		"io.session.end",
		"cancel.request",
		"cancel.resume",
	}
}

// Init creates the hub, adapter, and server, then wires event handlers.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.system = ctx.System

	// Read config.
	p.host = "localhost"
	if h, ok := ctx.Config["host"].(string); ok {
		p.host = h
	}
	p.port = 8080
	if port, ok := ctx.Config["port"].(int); ok {
		p.port = port
	}
	p.openBrowser = true
	if ob, ok := ctx.Config["open_browser"].(bool); ok {
		p.openBrowser = ob
	}

	// Create components.
	p.hub = NewHub(p.logger)

	sessionID := ""
	if ctx.Session != nil {
		sessionID = ctx.Session.ID
	}
	p.adapter = NewAdapter(p.hub, sessionID)
	p.server = NewServer(p.hub, ctx.Session, p.logger, p.host, p.port)

	// Wire inbound callbacks (user -> engine).
	// Input handling must run in a goroutine because the event bus dispatches
	// synchronously. If the agent loop invokes the ask tool, it blocks the
	// current goroutine waiting for a WebSocket response. Running the handler
	// on the readPump goroutine would deadlock since readPump cannot read the
	// answer while it is blocked inside the handler chain.
	p.adapter.OnInput(func(msg ui.InputMessage) {
		if msg.Content == "/quit" || msg.Content == "/exit" {
			_ = p.bus.Emit("io.session.end", events.SessionInfo{
				Transport: "browser",
			})
			return
		}
		go p.bus.Emit("io.input", events.UserInput{
			Content: msg.Content,
		})
	})

	p.adapter.OnApprovalResponse(func(msg ui.ApprovalResponseMessage) {
		_ = p.bus.Emit("io.approval.response", events.ApprovalResponse{
			PromptID: msg.PromptID,
			Approved: msg.Approved,
			Always:   msg.Always,
		})
	})

	p.adapter.OnCancel(func() {
		_ = p.bus.Emit("cancel.request", events.CancelRequest{
			Source: "browser",
		})
	})

	p.adapter.OnResume(func() {
		_ = p.bus.Emit("cancel.resume", events.CancelResume{})
	})

	// Wire outbound handlers (engine -> user).
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output.stream", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output.clear", p.handleOutputClear, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.status", p.handleStatus, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.ask", p.handleAskUser, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinkingStep, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.request", p.handlePlanApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.created", p.handlePlanCreated, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.plan", p.handleAgentPlan, engine.WithSource(pluginID)),
		p.bus.Subscribe("provider.fallback", p.handleProviderFallback, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.created", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.updated", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.history.replay", p.handleHistoryReplay, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
	)

	p.logger.Info("browser IO plugin initialized")
	return nil
}

// Ready starts the HTTP server and optionally opens the browser.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting browser server: %w", err)
	}

	_ = p.bus.Emit("io.session.start", events.SessionInfo{
		Transport: "browser",
	})

	if p.openBrowser && p.system != nil && p.system.HasOpen() {
		url := fmt.Sprintf("http://%s:%d", p.host, p.port)
		go func() {
			args := make([]string, len(p.system.OpenArgs), len(p.system.OpenArgs)+1)
			copy(args, p.system.OpenArgs)
			args = append(args, url)
			_ = exec.Command(p.system.OpenCmd, args...).Start()
		}()
	}

	return nil
}

// Shutdown stops the server and unsubscribes from events.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}

	// Close all WebSocket connections first so the HTTP server
	// doesn't block waiting for them to finish.
	p.hub.Close()

	// Use a fresh context with a deadline for the HTTP server shutdown
	// since the incoming context may already be cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.server.Shutdown(shutdownCtx)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	if streamed, _ := out.Metadata["streamed"].(bool); streamed {
		return
	}
	_ = p.adapter.SendOutput(ui.OutputMessage{
		Content:  out.Content,
		Role:     out.Role,
		Metadata: out.Metadata,
		TurnID:   out.TurnID,
	})
}

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.OutputChunk)
	if !ok {
		return
	}
	_ = p.adapter.SendStreamChunk(ui.StreamChunkMessage{
		Content: chunk.Content,
		TurnID:  chunk.TurnID,
		Index:   chunk.Index,
	})
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	ref, ok := e.Payload.(events.StreamRef)
	if !ok {
		return
	}
	_ = p.adapter.SendStreamEnd(ui.StreamEndMessage{
		TurnID:   ref.TurnID,
		Metadata: ref.Metadata,
	})
}

func (p *Plugin) handleOutputClear(_ engine.Event[any]) {
	_ = p.adapter.broadcast("output_clear", nil)
}

func (p *Plugin) handleProviderFallback(e engine.Event[any]) {
	fb, ok := e.Payload.(events.ProviderFallback)
	if !ok {
		return
	}
	_ = p.adapter.broadcast("provider_fallback", fb)
}

func (p *Plugin) handleStatus(e engine.Event[any]) {
	status, ok := e.Payload.(events.StatusUpdate)
	if !ok {
		return
	}
	_ = p.adapter.SendStatus(ui.StatusMessage{
		State:  status.State,
		Detail: status.Detail,
		ToolID: status.ToolID,
	})
}

func (p *Plugin) handleApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	_, err := p.adapter.RequestApproval(ui.ApprovalRequestMessage{
		PromptID:    req.PromptID,
		Description: req.Description,
		ToolCall:    req.ToolCall,
		Risk:        req.Risk,
	})
	if err != nil {
		p.logger.Error("approval request failed", "error", err)
	}
}

func (p *Plugin) handleAskUser(e engine.Event[any]) {
	ask, ok := e.Payload.(events.AskUser)
	if !ok {
		return
	}
	resp, err := p.adapter.RequestInput(ui.AskUserMessage{
		PromptID: ask.PromptID,
		Question: ask.Question,
		TurnID:   ask.TurnID,
	})
	if err != nil {
		p.logger.Error("ask user request failed", "error", err)
		return
	}
	_ = p.bus.Emit("io.ask.response", events.AskUserResponse{
		PromptID: resp.PromptID,
		Answer:   resp.Answer,
	})
}

func (p *Plugin) handleThinkingStep(e engine.Event[any]) {
	step, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	_ = p.adapter.SendThinking(ui.ThinkingMessage{
		Content: step.Content,
		Phase:   step.Phase,
		Source:  step.Source,
		TurnID:  step.TurnID,
	})
}

func (p *Plugin) handlePlanApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	resp, err := p.adapter.RequestApproval(ui.ApprovalRequestMessage{
		PromptID:    req.PromptID,
		Description: req.Description,
		ToolCall:    req.ToolCall,
		Risk:        req.Risk,
	})
	if err != nil {
		p.logger.Error("plan approval request failed", "error", err)
		return
	}
	_ = p.bus.Emit("plan.approval.response", events.ApprovalResponse{
		PromptID: resp.PromptID,
		Approved: resp.Approved,
		Always:   resp.Always,
	})
}

func (p *Plugin) handlePlanCreated(e engine.Event[any]) {
	result, ok := e.Payload.(events.PlanResult)
	if !ok {
		return
	}
	steps := make([]ui.PlanDisplayStep, len(result.Steps))
	for i, s := range result.Steps {
		steps[i] = ui.PlanDisplayStep{
			ID:          s.ID,
			Description: s.Description,
			Status:      s.Status,
			Order:       s.Order,
		}
	}
	_ = p.adapter.SendPlanDisplay(ui.PlanDisplayMessage{
		PlanID:  result.PlanID,
		Summary: result.Summary,
		Steps:   steps,
		Source:  result.Source,
		TurnID:  result.TurnID,
	})
}

func (p *Plugin) handleAgentPlan(e engine.Event[any]) {
	plan, ok := e.Payload.(events.Plan)
	if !ok {
		return
	}
	steps := make([]ui.PlanDisplayStep, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = ui.PlanDisplayStep{
			ID:          fmt.Sprintf("step_%d", i+1),
			Description: s.Description,
			Status:      s.Status,
			Order:       i + 1,
		}
	}
	_ = p.adapter.SendPlanUpdate(ui.PlanDisplayMessage{
		Steps:  steps,
		TurnID: plan.TurnID,
		Source:  "agent",
	})
}

func (p *Plugin) handleFileChanged(e engine.Event[any]) {
	data, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	path, _ := data["path"].(string)
	action := "updated"
	if e.Type == "session.file.created" {
		action = "created"
	}
	_ = p.adapter.SendFileChanged(path, action)
}

func (p *Plugin) handleCancelComplete(e engine.Event[any]) {
	cc, ok := e.Payload.(events.CancelComplete)
	if !ok {
		return
	}
	_ = p.adapter.SendCancelComplete(cc.TurnID, cc.Resumable)
}

func (p *Plugin) handleHistoryReplay(e engine.Event[any]) {
	replay, ok := e.Payload.(events.HistoryReplay)
	if !ok {
		return
	}
	for _, msg := range replay.Messages {
		role := msg.Role
		switch role {
		case "user", "assistant", "system", "error":
			// These are directly displayable.
		case "tool_invoke", "tool_result":
			continue
		default:
			continue
		}
		_ = p.adapter.SendOutput(ui.OutputMessage{
			Content: msg.Content,
			Role:    role,
		})
	}
}

