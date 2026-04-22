package tui

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/ui"
)

const pluginID = "nexus.io.tui"

// Plugin is the terminal IO plugin that bridges a BubbleTea TUI to the event bus.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	adapter *Adapter
	unsubs  []func()
}

// New creates a new terminal IO plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Terminal IO" }
func (p *Plugin) Version() string                   { return "0.2.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "io.output.clear", Priority: 50},
		{EventType: "io.status", Priority: 50},
		{EventType: "io.approval.request", Priority: 50},
		{EventType: "io.ask", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		{EventType: "code.exec.stdout", Priority: 50},
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
		"before:io.input",
		"io.approval.response",
		"io.ask.response",
		"plan.approval.response",
		"io.session.start",
		"io.session.end",
		"cancel.request",
		"cancel.resume",
	}
}

// Init creates the adapter and wires callbacks to the event bus.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.adapter = NewAdapter(ctx.Session, ctx.Capabilities)

	// Wire inbound callbacks (user -> engine).
	p.adapter.OnInput(func(msg ui.InputMessage) {
		if msg.Content == "/quit" || msg.Content == "/exit" {
			_ = p.bus.Emit("io.session.end", events.SessionInfo{
				Transport: "tui",
			})
			return
		}
		input := events.UserInput{Content: msg.Content}
		if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
			return
		}
		_ = p.bus.Emit("io.input", input)
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
			Source: "tui",
		})
	})

	p.adapter.OnResume(func() {
		_ = p.bus.Emit("cancel.resume", events.CancelResume{})
	})

	// Wire outbound handlers (engine -> user).
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output.clear", p.handleOutputClear, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.status", p.handleStatus, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.ask", p.handleAskUser, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinkingStep, engine.WithSource(pluginID)),
		p.bus.Subscribe("code.exec.stdout", p.handleCodeExecStdout, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.request", p.handlePlanApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.created", p.handlePlanCreated, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.plan", p.handleAgentPlan, engine.WithSource(pluginID)),
		p.bus.Subscribe("provider.fallback", p.handleProviderFallback, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.created", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.updated", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.history.replay", p.handleHistoryReplay, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
	)

	p.logger.Info("terminal IO plugin initialized")
	return nil
}

// Ready starts the BubbleTea program.
func (p *Plugin) Ready() error {
	_ = p.bus.Emit("io.session.start", events.SessionInfo{
		Transport: "tui",
	})
	return p.adapter.Start(context.Background())
}

// Shutdown stops the adapter and unsubscribes from events.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return p.adapter.Stop(ctx)
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
	chunk, ok := e.Payload.(events.StreamChunk)
	if !ok || chunk.Content == "" {
		return
	}
	_ = p.adapter.SendStreamChunk(ui.StreamChunkMessage{
		Content: chunk.Content,
		TurnID:  chunk.TurnID,
		Index:   chunk.Index,
	})
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	_ = p.adapter.SendStreamEnd(ui.StreamEndMessage{
		TurnID: end.TurnID,
	})
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

func (p *Plugin) handleCodeExecStdout(e engine.Event[any]) {
	out, ok := e.Payload.(events.CodeExecStdout)
	if !ok {
		return
	}
	_ = p.adapter.SendCodeExecStdout(ui.CodeExecStdoutMessage{
		CallID:    out.CallID,
		TurnID:    out.TurnID,
		Chunk:     out.Chunk,
		Final:     out.Final,
		Truncated: out.Truncated,
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
	steps := make([]planUpdateStep, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = planUpdateStep{
			Description: s.Description,
			Status:      s.Status,
		}
	}
	_ = p.adapter.SendPlanUpdate(plan.TurnID, steps)
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
	if cc.Resumable {
		_ = p.adapter.SendCancelComplete(cc.TurnID, true)
	}
}

func (p *Plugin) handleOutputClear(_ engine.Event[any]) {
	if p.adapter.program != nil {
		p.adapter.program.Send(outputClearMsg{})
	}
}

func (p *Plugin) handleProviderFallback(e engine.Event[any]) {
	fb, ok := e.Payload.(events.ProviderFallback)
	if !ok {
		return
	}
	msg := fmt.Sprintf("Provider %s unavailable — switching to %s (%s)",
		fb.FailedProvider, fb.NextProvider, fb.NextModel)
	_ = p.adapter.SendOutput(ui.OutputMessage{
		Content: msg,
		Role:    "system",
	})
}

func (p *Plugin) handleHistoryReplay(e engine.Event[any]) {
	replay, ok := e.Payload.(events.HistoryReplay)
	if !ok {
		return
	}
	for _, msg := range replay.Messages {
		role := msg.Role
		// Map conversation memory roles to display roles.
		switch role {
		case "user", "assistant", "system", "error":
			// These are directly displayable.
		case "tool_invoke", "tool_result":
			// Skip intermediate tool messages in the replay.
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
