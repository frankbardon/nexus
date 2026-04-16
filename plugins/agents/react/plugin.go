package react

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.agent.react"
	pluginName = "ReAct Agent"
	version    = "0.1.0"
)

// Plugin implements the ReAct (Reason + Act) agent loop.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	systemPrompt     string
	systemPromptFile string
	planningEnabled  bool
	modelRole        string

	mu               sync.Mutex
	history          []events.Message
	registeredTools  []events.ToolDef
	skillContexts    []string
	currentTurnID    string
	currentPlan      *events.PlanResult
	currentPlanStep  int // index into currentPlan.Steps; -1 when no plan is active
	iteration        int
	pendingToolCalls int
	streamed         bool
	cancelled        bool
	unsubs           []func()
}

// New creates a new ReAct agent plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if pe, ok := ctx.Config["planning"].(bool); ok {
		p.planningEnabled = pe
	}

	if mr, ok := ctx.Config["model_role"].(string); ok {
		p.modelRole = mr
	}

	if spf, ok := ctx.Config["system_prompt_file"].(string); ok {
		p.systemPromptFile = spf
		data, err := os.ReadFile(spf)
		if err != nil {
			return fmt.Errorf("react: failed to read system prompt file %s: %w", spf, err)
		}
		p.systemPrompt = string(data)
	}

	if sp, ok := ctx.Config["system_prompt"].(string); ok {
		p.systemPrompt = sp
	}

	// Register event handlers.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInputEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResultEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponseEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunkEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEndEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.loaded", p.handleSkillLoadedEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.register", p.handleToolRegisterEvent,
			engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.result", p.handlePlanResultEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.active", p.handleCancelEvent,
			engine.WithPriority(20), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.resume", p.handleResumeEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("memory.compacted", p.handleCompactedEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("gate.llm.retry", p.handleGateRetry,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

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
		{EventType: "io.input", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "llm.response", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "skill.loaded", Priority: 50},
		{EventType: "plan.result", Priority: 50},
		{EventType: "cancel.active", Priority: 20},
		{EventType: "cancel.resume", Priority: 50},
		{EventType: "memory.compacted", Priority: 50},
		{EventType: "gate.llm.retry", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"before:llm.request",
		"before:tool.invoke",
		"tool.invoke",
		"before:tool.result",
		"tool.result",
		"before:io.output",
		"io.output",
		"io.output.stream",
		"io.output.stream.end",
		"io.status",
		"agent.turn.start",
		"agent.turn.end",
		"agent.plan",
		"plan.request",
		"skill.activate",
		"cancel.complete",
	}
}

// Event handler wrappers.

func (p *Plugin) handleInputEvent(event engine.Event[any]) {
	if input, ok := event.Payload.(events.UserInput); ok {
		p.handleInput(input)
	}
}

func (p *Plugin) handleLLMResponseEvent(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	// Skip responses tagged for other plugins (e.g., planner LLM calls).
	if source, _ := resp.Metadata["_source"].(string); source != "" {
		return
	}
	p.handleLLMResponse(resp)
}

func (p *Plugin) handlePlanResultEvent(event engine.Event[any]) {
	result, ok := event.Payload.(events.PlanResult)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.currentPlan != nil {
		p.mu.Unlock()
		return // already have a plan for this turn
	}
	p.currentPlan = &result
	p.currentPlanStep = 0
	p.mu.Unlock()

	if !result.Approved || len(result.Steps) == 0 {
		// Plan was rejected or empty — emit output and end turn.
		_ = p.bus.Emit("io.output", events.AgentOutput{
			Content: "Plan was not approved. Please try again with a different request.",
			Role:    "system",
			TurnID:  result.TurnID,
		})
		_ = p.bus.Emit("agent.turn.end", events.TurnInfo{
			TurnID: result.TurnID,
		})
		return
	}

	p.emitPlanProgress()
	p.emitStatus("thinking", fmt.Sprintf("Plan ready, executing step 1/%d", len(result.Steps)))
	p.sendLLMRequest()
}

func (p *Plugin) handleToolResultEvent(event engine.Event[any]) {
	if result, ok := event.Payload.(events.ToolResult); ok {
		p.handleToolResult(result)
	}
}

func (p *Plugin) handleStreamChunkEvent(event engine.Event[any]) {
	chunk, ok := event.Payload.(events.StreamChunk)
	if !ok || chunk.Content == "" {
		return
	}
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return
	}
	p.streamed = true
	p.mu.Unlock()
	_ = p.bus.Emit("io.output.stream", events.OutputChunk{
		Content: chunk.Content,
		TurnID:  chunk.TurnID,
		Index:   chunk.Index,
	})
}

func (p *Plugin) handleStreamEndEvent(event engine.Event[any]) {
	end, ok := event.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	_ = p.bus.Emit("io.output.stream.end", events.StreamRef{
		TurnID: end.TurnID,
	})
}

func (p *Plugin) handleSkillLoadedEvent(event engine.Event[any]) {
	if content, ok := event.Payload.(events.SkillContent); ok {
		p.handleSkillLoaded(content)
	}
}

func (p *Plugin) handleToolRegisterEvent(event engine.Event[any]) {
	td, ok := event.Payload.(events.ToolDef)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registeredTools = append(p.registeredTools, td)
	p.logger.Info("registered tool", "name", td.Name)
}

// Cancel/resume handlers.

func (p *Plugin) handleCancelEvent(event engine.Event[any]) {
	cancel, ok := event.Payload.(events.CancelActive)
	if !ok {
		return
	}

	p.mu.Lock()
	if p.currentTurnID == "" || p.currentTurnID != cancel.TurnID {
		p.mu.Unlock()
		return
	}

	turnID := p.currentTurnID
	p.cancelled = true
	p.pendingToolCalls = 0
	p.mu.Unlock()

	p.logger.Info("turn cancelled", "turn_id", turnID)

	p.emitStatus("idle", "")

	_ = p.bus.Emit("io.output", events.AgentOutput{
		Content: "_Operation cancelled. Type /resume or press the resume button to continue._",
		Role:    "system",
		TurnID:  turnID,
	})

	_ = p.bus.Emit("cancel.complete", events.CancelComplete{
		TurnID:    turnID,
		Resumable: true,
	})

	_ = p.bus.Emit("agent.turn.end", events.TurnInfo{
		TurnID:    turnID,
		Iteration: p.iteration,
	})
}

func (p *Plugin) handleResumeEvent(event engine.Event[any]) {
	if _, ok := event.Payload.(events.CancelResume); !ok {
		return
	}

	p.mu.Lock()
	if !p.cancelled {
		p.mu.Unlock()
		return
	}

	p.cancelled = false
	turnID := p.currentTurnID
	p.mu.Unlock()

	p.logger.Info("resuming cancelled turn", "turn_id", turnID)

	_ = p.bus.Emit("agent.turn.start", events.TurnInfo{
		TurnID:    turnID,
		Iteration: p.iteration,
	})

	p.emitStatus("thinking", "Resuming cancelled operation")
	p.sendLLMRequest()
}

func (p *Plugin) handleCompactedEvent(event engine.Event[any]) {
	cc, ok := event.Payload.(events.CompactionComplete)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = make([]events.Message, len(cc.Messages))
	copy(p.history, cc.Messages)
	p.logger.Info("history replaced by compaction", "messages", len(cc.Messages))
}

// handleGateRetry is called when a gate (rate limiter, context window, etc.)
// signals that a previously vetoed LLM request should be retried.
func (p *Plugin) handleGateRetry(_ engine.Event[any]) {
	p.mu.Lock()
	if p.currentTurnID == "" || p.cancelled {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	p.logger.Info("gate.llm.retry received, re-sending LLM request")
	p.sendLLMRequest()
}

// Core logic.

func (p *Plugin) handleInput(input events.UserInput) {
	// Handle /resume command.
	if strings.TrimSpace(input.Content) == "/resume" {
		p.mu.Lock()
		if !p.cancelled {
			p.mu.Unlock()
			_ = p.bus.Emit("io.output", events.AgentOutput{
				Content: "Nothing to resume.",
				Role:    "system",
			})
			return
		}
		turnID := p.currentTurnID
		p.mu.Unlock()

		_ = p.bus.Emit("cancel.resume", events.CancelResume{
			TurnID: turnID,
		})
		return
	}

	p.mu.Lock()

	// Start a new turn.
	p.currentTurnID = generateTurnID()
	p.currentPlan = nil
	p.currentPlanStep = -1
	p.iteration = 0
	p.pendingToolCalls = 0
	p.streamed = false

	// Add user message to history.
	p.history = append(p.history, events.Message{
		Role:    "user",
		Content: input.Content,
	})

	turnID := p.currentTurnID
	p.mu.Unlock()

	// Emit turn start.
	_ = p.bus.Emit("agent.turn.start", events.TurnInfo{
		TurnID:    turnID,
		Iteration: 0,
		SessionID: input.SessionID,
	})

	if p.planningEnabled {
		p.emitStatus("thinking", "Requesting plan")
		_ = p.bus.Emit("plan.request", events.PlanRequest{
			TurnID:    turnID,
			SessionID: input.SessionID,
			Input:     input.Content,
		})
	} else {
		p.emitStatus("thinking", "Processing input")
		p.sendLLMRequest()
	}
}

func (p *Plugin) handleLLMResponse(resp events.LLMResponse) {
	p.mu.Lock()

	if p.cancelled {
		p.mu.Unlock()
		return
	}

	// Add assistant message to history.
	assistantMsg := events.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	}
	p.history = append(p.history, assistantMsg)
	p.iteration++

	turnID := p.currentTurnID
	iteration := p.iteration

	if len(resp.ToolCalls) > 0 {
		// Iteration limiting now handled by nexus.gate.endless_loop plugin.
		p.pendingToolCalls = len(resp.ToolCalls)
		p.mu.Unlock()

		// Emit agent.plan describing the tool calls as plan steps.
		steps := make([]events.PlanStep, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			steps[i] = events.PlanStep{
				Description: fmt.Sprintf("Run tool: %s", tc.Name),
				Status:      "pending",
			}
		}
		_ = p.bus.Emit("agent.plan", events.Plan{
			Steps:  steps,
			TurnID: turnID,
		})

		p.emitStatus("tool_running", fmt.Sprintf("Running %d tool(s)", len(resp.ToolCalls)))

		// Invoke each tool call with vetoable before:tool.invoke.
		for _, tc := range resp.ToolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				args = map[string]any{}
			}

			toolCall := events.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: args,
				TurnID:    turnID,
			}

			if veto, err := p.bus.EmitVetoable("before:tool.invoke", &toolCall); err == nil && veto.Vetoed {
				p.logger.Info("tool.invoke vetoed", "tool", tc.Name, "reason", veto.Reason)
				// Emit a synthetic tool result so the agent loop can continue.
				syntheticResult := events.ToolResult{
					ID:     tc.ID,
					Name:   tc.Name,
					Error:  fmt.Sprintf("Tool call vetoed: %s", veto.Reason),
					TurnID: turnID,
				}
				if rv, rvErr := p.bus.EmitVetoable("before:tool.result", &syntheticResult); rvErr == nil && rv.Vetoed {
					p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", rv.Reason)
					continue
				}
				_ = p.bus.Emit("tool.result", syntheticResult)
				continue
			}

			_ = p.bus.Emit("tool.invoke", toolCall)
		}
		return
	}

	// No tool calls: check if there are remaining plan steps before finishing.
	if p.currentPlan != nil && p.currentPlanStep >= 0 {
		// Mark the current step as completed.
		if p.currentPlanStep < len(p.currentPlan.Steps) {
			p.currentPlan.Steps[p.currentPlanStep].Status = "completed"
		}

		p.currentPlanStep++
		if p.currentPlanStep < len(p.currentPlan.Steps) {
			// More steps remain — advance and continue the loop.
			stepIdx := p.currentPlanStep
			p.mu.Unlock()

			p.emitPlanProgress()
			p.emitStatus("thinking", fmt.Sprintf("Plan step %d/%d", stepIdx+1, len(p.currentPlan.Steps)))
			p.sendLLMRequest()
			return
		}
		// All steps completed — emit final progress update, then fall through to output.
		p.mu.Unlock()
		p.emitPlanProgress()
		p.mu.Lock()
	}

	streamed := p.streamed
	p.streamed = false
	p.mu.Unlock()

	p.emitStatus("idle", "")

	// Emit vetoable before:io.output.
	output := events.AgentOutput{
		Content:  resp.Content,
		Role:     "assistant",
		TurnID:   turnID,
		Metadata: map[string]any{"streamed": streamed},
	}
	if veto, err := p.bus.EmitVetoable("before:io.output", &output); err == nil && veto.Vetoed {
		p.logger.Info("io.output vetoed", "reason", veto.Reason)
	} else {
		_ = p.bus.Emit("io.output", output)
	}
	p.mu.Lock()
	p.currentPlan = nil
	p.currentPlanStep = -1
	p.mu.Unlock()

	_ = p.bus.Emit("agent.turn.end", events.TurnInfo{
		TurnID:    turnID,
		Iteration: iteration,
	})
}

func (p *Plugin) handleToolResult(result events.ToolResult) {
	p.mu.Lock()

	// Ignore tool results from other turns (e.g. subagent tool calls).
	if result.TurnID != "" && result.TurnID != p.currentTurnID {
		p.mu.Unlock()
		return
	}

	// Build content for the tool result message.
	content := result.Output
	if result.Error != "" {
		content = "Error: " + result.Error
	}

	// Add tool result to history.
	p.history = append(p.history, events.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: result.ID,
	})

	p.pendingToolCalls--
	allDone := p.pendingToolCalls <= 0
	p.mu.Unlock()

	// Only send the next LLM request once all tool results are in.
	if allDone {
		p.emitStatus("thinking", "Processing tool results")
		p.sendLLMRequest()
	}
}

func (p *Plugin) handleSkillLoaded(content events.SkillContent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skillContexts = append(p.skillContexts, fmt.Sprintf("## Skill: %s\n\n%s", content.Name, content.Body))
	p.logger.Info("loaded skill context", "name", content.Name)
}

func (p *Plugin) sendLLMRequest() {
	p.mu.Lock()

	// Build system prompt with skill contexts and plan.
	systemPrompt := p.systemPrompt
	if len(p.skillContexts) > 0 {
		systemPrompt += "\n\n---\n\n" + strings.Join(p.skillContexts, "\n\n---\n\n")
	}
	if p.currentPlan != nil && len(p.currentPlan.Steps) > 0 && p.currentPlanStep >= 0 {
		var planText strings.Builder
		planText.WriteString("\n\n## Execution Plan\n\n")
		planText.WriteString(p.currentPlan.Summary)
		planText.WriteString("\n\nFull plan:\n")
		for i, step := range p.currentPlan.Steps {
			marker := "  "
			if i < p.currentPlanStep {
				marker = "\u2713 " // checkmark for completed
			} else if i == p.currentPlanStep {
				marker = "> " // arrow for current
			}
			fmt.Fprintf(&planText, "%sStep %d: %s\n", marker, step.Order, step.Description)
		}

		current := p.currentPlan.Steps[p.currentPlanStep]
		instructions := current.Instructions
		if instructions == "" {
			instructions = current.Description
		}
		fmt.Fprintf(&planText, "\n## CURRENT TASK (Step %d of %d)\n\n", current.Order, len(p.currentPlan.Steps))
		fmt.Fprintf(&planText, "%s\n\n", instructions)
		planText.WriteString("You MUST focus exclusively on this step. Do not skip ahead or work on other steps. ")
		planText.WriteString("When this step is complete, respond with your results — do not call any more tools.\n")
		systemPrompt += planText.String()
	}

	// Build messages: system prompt + conversation history.
	var messages []events.Message
	if systemPrompt != "" {
		messages = append(messages, events.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	messages = append(messages, p.history...)

	// Copy registered tools.
	tools := make([]events.ToolDef, len(p.registeredTools))
	copy(tools, p.registeredTools)

	p.mu.Unlock()

	req := events.LLMRequest{
		Role:     p.modelRole,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
	}

	if veto, err := p.bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", req)
}

// emitPlanProgress emits an agent.plan event reflecting current step progress.
func (p *Plugin) emitPlanProgress() {
	p.mu.Lock()
	plan := p.currentPlan
	stepIdx := p.currentPlanStep
	turnID := p.currentTurnID
	p.mu.Unlock()

	if plan == nil || len(plan.Steps) == 0 {
		return
	}

	steps := make([]events.PlanStep, len(plan.Steps))
	for i, s := range plan.Steps {
		status := s.Status
		if i < stepIdx {
			status = "completed"
		} else if i == stepIdx {
			status = "active"
		}
		steps[i] = events.PlanStep{
			Description: s.Description,
			Status:      status,
		}
	}

	_ = p.bus.Emit("agent.plan", events.Plan{
		Steps:  steps,
		TurnID: turnID,
	})
}

func (p *Plugin) emitStatus(state, detail string) {
	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  state,
		Detail: detail,
	})
}

// generateTurnID produces a unique turn identifier.
func generateTurnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("turn_%x", b)
}
