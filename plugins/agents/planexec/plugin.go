package planexec

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.agent.planexec"
	pluginName = "Plan and Execute Agent"
	version    = "0.2.0"

	executorSource    = "nexus.agent.planexec"
	synthesizerSource = "nexus.agent.planexec.synthesizer"
)

// phase represents the current phase of the agent's lifecycle.
type phase int

const (
	phaseIdle phase = iota
	phasePlanning
	phaseAwaitingApproval
	phaseExecuting
	phaseSynthesizing
)

func (p phase) String() string {
	switch p {
	case phaseIdle:
		return "idle"
	case phasePlanning:
		return "planning"
	case phaseAwaitingApproval:
		return "awaiting_approval"
	case phaseExecuting:
		return "executing"
	case phaseSynthesizing:
		return "synthesizing"
	default:
		return "unknown"
	}
}

// planStep holds runtime state for a single step in the execution plan.
type planStep struct {
	ID           string `json:"id"`
	Description  string `json:"description"`
	Instructions string `json:"instructions,omitempty"`
	Status       string `json:"status"` // "pending", "active", "completed", "failed"
	Result       string `json:"result,omitempty"`
}

// Plugin implements the Plan and Execute agent loop. Planning is delegated to
// whichever planner plugin is configured on the bus (e.g. nexus.planner.dynamic
// or nexus.planner.static) via the plan.request / plan.result event contract.
// planexec retains full control of the surrounding control flow: phase
// transitions, approval, step execution, re-planning on failure, and
// synthesis.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	systemPrompt       string
	systemPromptFile   string
	executionModelRole string
	replanOnFailure    bool
	approval           string // "always", "never"

	mu               sync.Mutex
	phase            phase
	history          []events.Message
	registeredTools  []events.ToolDef
	skillContexts    []string
	currentTurnID    string
	currentSessionID string
	iteration        int
	pendingToolCalls int
	streamed         bool

	// Plan state.
	planID          string
	plan            []planStep
	currentStepIdx  int
	stepHistory     []events.Message  // history scoped to the current step
	stepResults     map[string]string // stepID -> result summary
	originalInput   string
	replanCount     int
	pendingApproval bool

	unsubs []func()
}

// New creates a new Plan and Execute agent plugin.
func New() engine.Plugin {
	return &Plugin{
		executionModelRole: "balanced",
		replanOnFailure:    true,
		approval:           "never",
		stepResults:        make(map[string]string),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if mr, ok := ctx.Config["execution_model_role"].(string); ok {
		p.executionModelRole = mr
	}

	if rpf, ok := ctx.Config["replan_on_failure"].(bool); ok {
		p.replanOnFailure = rpf
	}

	if ap, ok := ctx.Config["approval"].(string); ok {
		p.approval = ap
	}

	if spf, ok := ctx.Config["system_prompt_file"].(string); ok && spf != "" {
		p.systemPromptFile = spf
		data, err := os.ReadFile(spf)
		if err != nil {
			return fmt.Errorf("planexec: failed to read system prompt file %s: %w", spf, err)
		}
		p.systemPrompt = string(data)
	}

	if sp, ok := ctx.Config["system_prompt"].(string); ok && sp != "" {
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
		p.bus.Subscribe("plan.approval.response", p.handlePlanApprovalResponseEvent,
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
		{EventType: "plan.approval.response", Priority: 50},
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
		"plan.approval.request",
		"thinking.step",
		"skill.activate",
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
	source, _ := resp.Metadata["_source"].(string)
	switch source {
	case synthesizerSource:
		p.handleSynthesizerResponse(resp)
	case executorSource:
		p.handleExecutorResponse(resp)
	default:
		// Not tagged for this plugin — ignore.
	}
}

// handlePlanResultEvent receives a plan produced by the configured planner
// plugin. planexec retains control of the surrounding flow and only uses the
// planner to generate the step list.
func (p *Plugin) handlePlanResultEvent(event engine.Event[any]) {
	result, ok := event.Payload.(events.PlanResult)
	if !ok {
		return
	}

	p.mu.Lock()
	if p.phase != phasePlanning {
		p.mu.Unlock()
		return
	}
	// Only accept results for the current turn (empty TurnID means planner
	// did not tag, accept optimistically).
	if result.TurnID != "" && result.TurnID != p.currentTurnID {
		p.mu.Unlock()
		return
	}
	turnID := p.currentTurnID
	p.mu.Unlock()

	// If the planner ran its own approval flow and it was denied, short
	// circuit the turn.
	if !result.Approved {
		p.logger.Info("planner returned unapproved plan", "turn", turnID, "source", result.Source)
		p.finishTurnWithMessage(turnID, "Plan was not approved.")
		return
	}

	if len(result.Steps) == 0 {
		p.logger.Warn("planner returned empty plan", "turn", turnID, "source", result.Source)
		p.finishTurnWithMessage(turnID, "Planner returned an empty plan.")
		return
	}

	// Convert PlanResultStep -> internal planStep.
	steps := make([]planStep, len(result.Steps))
	for i, s := range result.Steps {
		id := s.ID
		if id == "" {
			id = fmt.Sprintf("step_%d", i+1)
		}
		steps[i] = planStep{
			ID:           id,
			Description:  s.Description,
			Instructions: s.Instructions,
			Status:       "pending",
		}
	}

	p.mu.Lock()
	p.planID = result.PlanID
	if p.planID == "" {
		p.planID = generatePlanID()
	}
	p.plan = steps
	p.currentStepIdx = 0
	needsApproval := p.approval == "always"
	if needsApproval {
		p.phase = phaseAwaitingApproval
		p.pendingApproval = true
	} else {
		p.phase = phaseExecuting
	}
	p.mu.Unlock()

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{
		TurnID:    turnID,
		Source:    pluginID,
		Content:   fmt.Sprintf("Plan received from %s (%d steps)", result.Source, len(steps)),
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	p.emitPlanUpdate(turnID)
	p.emitStatus("thinking", fmt.Sprintf("Plan received: %d steps", len(steps)))
	p.logger.Info("plan received", "steps", len(steps), "turn", turnID, "source", result.Source)

	if needsApproval {
		p.emitStatus("waiting", "Awaiting plan approval")
		_ = p.bus.Emit("plan.approval.request", events.ApprovalRequest{
			PromptID:    p.planID,
			Description: formatPlanForDisplay(steps),
			Risk:        "medium",
		})
		return
	}

	p.emitStatus("thinking", "Executing plan")
	p.executeNextStep()
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
	currentPhase := p.phase
	p.mu.Unlock()

	// Only stream output during the synthesizing phase to avoid leaking
	// intermediate step streams to the user.
	if currentPhase == phaseSynthesizing {
		p.mu.Lock()
		p.streamed = true
		p.mu.Unlock()
		_ = p.bus.Emit("io.output.stream", events.OutputChunk{
			Content: chunk.Content,
			TurnID:  chunk.TurnID,
			Index:   chunk.Index,
		})
	}
}

func (p *Plugin) handleStreamEndEvent(event engine.Event[any]) {
	end, ok := event.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	p.mu.Lock()
	currentPhase := p.phase
	p.mu.Unlock()

	if currentPhase == phaseSynthesizing {
		_ = p.bus.Emit("io.output.stream.end", events.StreamRef{
			TurnID: end.TurnID,
		})
	}
}

func (p *Plugin) handleSkillLoadedEvent(event engine.Event[any]) {
	if content, ok := event.Payload.(events.SkillContent); ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.skillContexts = append(p.skillContexts, engine.XMLWrap("skill", content.Body, "name", content.Name))
		p.logger.Info("loaded skill context", "name", content.Name)
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

func (p *Plugin) handlePlanApprovalResponseEvent(event engine.Event[any]) {
	resp, ok := event.Payload.(events.ApprovalResponse)
	if !ok {
		return
	}

	p.mu.Lock()
	// Only act on approvals we requested.
	if !p.pendingApproval || p.phase != phaseAwaitingApproval {
		p.mu.Unlock()
		return
	}
	if resp.PromptID != "" && resp.PromptID != p.planID {
		p.mu.Unlock()
		return
	}
	p.pendingApproval = false
	turnID := p.currentTurnID
	p.mu.Unlock()

	if resp.Approved {
		p.logger.Info("plan approved", "turn", turnID)
		p.mu.Lock()
		p.phase = phaseExecuting
		p.mu.Unlock()
		p.emitStatus("thinking", "Executing plan")
		p.executeNextStep()
	} else {
		p.logger.Info("plan denied", "turn", turnID)
		p.finishTurnWithMessage(turnID, "Plan was not approved.")
	}
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

// handleGateRetry is called when a gate signals that a previously vetoed LLM
// request should be retried (e.g. after rate limit window or compaction).
func (p *Plugin) handleGateRetry(_ engine.Event[any]) {
	p.mu.Lock()
	if p.currentTurnID == "" {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	p.logger.Info("gate.llm.retry received, re-sending LLM request")
	p.sendStepLLMRequest()
}

// Core logic.

func (p *Plugin) handleInput(input events.UserInput) {
	p.mu.Lock()

	// Start a new turn.
	p.currentTurnID = generateTurnID()
	p.currentSessionID = input.SessionID
	p.iteration = 0
	p.pendingToolCalls = 0
	p.streamed = false
	p.planID = ""
	p.plan = nil
	p.currentStepIdx = 0
	p.stepHistory = nil
	p.stepResults = make(map[string]string)
	p.originalInput = input.Content
	p.replanCount = 0
	p.pendingApproval = false
	p.phase = phasePlanning

	// Add user message to history.
	p.history = append(p.history, events.Message{
		Role:    "user",
		Content: input.Content,
	})

	turnID := p.currentTurnID
	sessionID := p.currentSessionID
	p.mu.Unlock()

	// Emit turn start.
	_ = p.bus.Emit("agent.turn.start", events.TurnInfo{
		TurnID:    turnID,
		Iteration: 0,
		SessionID: input.SessionID,
	})

	p.emitStatus("thinking", "Requesting plan")
	_ = p.bus.Emit("thinking.step", events.ThinkingStep{
		TurnID:    turnID,
		Source:    pluginID,
		Content:   "Requesting plan from configured planner...",
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	// Delegate plan generation to whichever planner plugin is active.
	_ = p.bus.Emit("plan.request", events.PlanRequest{
		TurnID:    turnID,
		SessionID: sessionID,
		Input:     input.Content,
	})
}

// finishTurnWithMessage emits a final system message and closes out the turn.
func (p *Plugin) finishTurnWithMessage(turnID, message string) {
	p.mu.Lock()
	p.phase = phaseIdle
	p.pendingApproval = false
	iteration := p.iteration
	p.mu.Unlock()

	_ = p.bus.Emit("io.output", events.AgentOutput{
		Content: message,
		Role:    "system",
		TurnID:  turnID,
	})
	p.emitStatus("idle", "")
	_ = p.bus.Emit("agent.turn.end", events.TurnInfo{
		TurnID:    turnID,
		Iteration: iteration,
	})
}

// executeNextStep begins execution of the next pending step.
func (p *Plugin) executeNextStep() {
	p.mu.Lock()

	if p.currentStepIdx >= len(p.plan) {
		// All steps complete — move to synthesis.
		p.phase = phaseSynthesizing
		p.mu.Unlock()
		p.emitStatus("thinking", "Synthesizing results")
		p.sendSynthesisRequest()
		return
	}

	step := &p.plan[p.currentStepIdx]
	step.Status = "active"
	p.iteration = 0
	p.pendingToolCalls = 0
	p.stepHistory = nil
	turnID := p.currentTurnID
	p.mu.Unlock()

	p.emitPlanUpdate(turnID)
	p.emitStatus("thinking", fmt.Sprintf("Step %d/%d: %s", p.currentStepIdx+1, len(p.plan), step.Description))
	p.logger.Info("executing step", "step_id", step.ID, "description", step.Description, "index", p.currentStepIdx+1, "total", len(p.plan))

	p.sendStepLLMRequest()
}

// sendStepLLMRequest sends an LLM request for the current step.
func (p *Plugin) sendStepLLMRequest() {
	p.mu.Lock()

	if p.currentStepIdx >= len(p.plan) {
		p.mu.Unlock()
		return
	}

	step := p.plan[p.currentStepIdx]

	// Build system prompt for step execution with XML boundaries.
	var systemPrompt strings.Builder
	if p.systemPrompt != "" {
		systemPrompt.WriteString(p.systemPrompt)
		systemPrompt.WriteString("\n\n")
	}
	if len(p.skillContexts) > 0 {
		systemPrompt.WriteString(engine.XMLWrap("skill_context", strings.Join(p.skillContexts, "\n")))
		systemPrompt.WriteString("\n")
	}

	var taskBody strings.Builder
	fmt.Fprintf(&taskBody, "You are executing step %d of %d in an execution plan.\n\n", p.currentStepIdx+1, len(p.plan))
	fmt.Fprintf(&taskBody, "Step: %s\n\n", step.Description)
	if step.Instructions != "" && step.Instructions != step.Description {
		fmt.Fprintf(&taskBody, "Instructions: %s\n\n", step.Instructions)
	}
	taskBody.WriteString("Focus on completing this specific step. Use the available tools as needed. ")
	taskBody.WriteString("When you have completed the step, provide a brief summary of what was accomplished.\n")
	systemPrompt.WriteString(engine.XMLWrap("current_task", taskBody.String()))

	systemPrompt.WriteString("\n")
	systemPrompt.WriteString(engine.XMLWrap("user_request", engine.XMLCDATA(p.originalInput)))

	// Include results from prior steps.
	if len(p.stepResults) > 0 {
		var priorBody strings.Builder
		for i := 0; i < p.currentStepIdx; i++ {
			prevStep := p.plan[i]
			if result, ok := p.stepResults[prevStep.ID]; ok {
				priorBody.WriteString(engine.XMLWrap("step_result",
					engine.XMLCDATA(result),
					"number", fmt.Sprintf("%d", i+1),
					"description", prevStep.Description))
			}
		}
		if priorBody.Len() > 0 {
			systemPrompt.WriteString("\n")
			systemPrompt.WriteString(engine.XMLWrap("prior_results", priorBody.String()))
		}
	}

	var messages []events.Message
	messages = append(messages, events.Message{
		Role:    "system",
		Content: systemPrompt.String(),
	})

	// Add the step-scoped conversation history.
	if len(p.stepHistory) == 0 {
		// First iteration for this step — add an initial user message.
		stepInstruction := step.Description
		if step.Instructions != "" {
			stepInstruction = step.Instructions
		}
		messages = append(messages, events.Message{
			Role:    "user",
			Content: fmt.Sprintf("Execute this step: %s", stepInstruction),
		})
	} else {
		messages = append(messages, p.stepHistory...)
	}

	tools := make([]events.ToolDef, len(p.registeredTools))
	copy(tools, p.registeredTools)

	p.mu.Unlock()

	req := events.LLMRequest{
		Role:     p.executionModelRole,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
		Metadata: map[string]any{
			"_source": executorSource,
		},
	}

	if veto, err := p.bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", req)
}

// handleExecutorResponse processes LLM responses during step execution.
func (p *Plugin) handleExecutorResponse(resp events.LLMResponse) {
	p.mu.Lock()

	if p.phase != phaseExecuting {
		p.mu.Unlock()
		return
	}

	// Add assistant message to step history.
	assistantMsg := events.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	}
	p.stepHistory = append(p.stepHistory, assistantMsg)
	p.iteration++

	turnID := p.currentTurnID
	stepIdx := p.currentStepIdx

	if len(resp.ToolCalls) > 0 {
		// Iteration limiting now handled by nexus.gate.endless_loop plugin.
		p.pendingToolCalls = len(resp.ToolCalls)
		p.mu.Unlock()

		p.emitStatus("tool_running", fmt.Sprintf("Step %d: Running %d tool(s)", stepIdx+1, len(resp.ToolCalls)))

		// Invoke each tool call.
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

	// No tool calls — step is complete.
	p.mu.Unlock()
	p.completeCurrentStep("completed", resp.Content)
}

// handleToolResult processes tool results during step execution.
func (p *Plugin) handleToolResult(result events.ToolResult) {
	p.mu.Lock()

	// Ignore tool results from other turns.
	if result.TurnID != "" && result.TurnID != p.currentTurnID {
		p.mu.Unlock()
		return
	}

	if p.phase != phaseExecuting {
		p.mu.Unlock()
		return
	}

	// Build content for the tool result message.
	content := result.Output
	if result.Error != "" {
		content = "Error: " + result.Error
	}

	// Add tool result to step history.
	p.stepHistory = append(p.stepHistory, events.Message{
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
		p.sendStepLLMRequest()
	}
}

// completeCurrentStep marks the current step as done and advances.
func (p *Plugin) completeCurrentStep(status, result string) {
	p.mu.Lock()

	if p.currentStepIdx >= len(p.plan) {
		p.mu.Unlock()
		return
	}

	step := &p.plan[p.currentStepIdx]
	step.Status = status
	step.Result = result
	p.stepResults[step.ID] = result

	turnID := p.currentTurnID
	stepIdx := p.currentStepIdx
	totalSteps := len(p.plan)
	failed := status == "failed"
	replanOnFailure := p.replanOnFailure
	replanCount := p.replanCount

	p.logger.Info("step completed",
		"step_id", step.ID,
		"status", status,
		"index", stepIdx+1,
		"total", totalSteps,
	)

	p.currentStepIdx++
	p.mu.Unlock()

	// Emit updated plan.
	p.emitPlanUpdate(turnID)

	// Handle failure with optional re-planning.
	if failed && replanOnFailure && replanCount < 2 {
		p.mu.Lock()
		p.phase = phasePlanning
		p.replanCount++
		p.mu.Unlock()

		p.emitStatus("thinking", "Step failed, re-planning")
		p.logger.Info("re-planning after step failure", "failed_step", stepIdx+1, "replan_count", replanCount+1)
		p.requestReplan()
		return
	}

	// Advance to the next step.
	p.executeNextStep()
}

// requestReplan emits a new plan.request to the configured planner, augmenting
// the original input with context about completed and failed steps. This keeps
// re-planning uniform across any planner implementation.
func (p *Plugin) requestReplan() {
	p.mu.Lock()

	var replanContext strings.Builder
	fmt.Fprintf(&replanContext, "Original request: %s\n\n", p.originalInput)
	replanContext.WriteString("Re-planning after a step failure. Status of previous plan:\n\n")
	for i, step := range p.plan {
		fmt.Fprintf(&replanContext, "- Step %d (%s): %s", i+1, step.ID, step.Description)
		switch step.Status {
		case "completed":
			fmt.Fprintf(&replanContext, " [COMPLETED] Result: %s", truncate(step.Result, 300))
		case "failed":
			fmt.Fprintf(&replanContext, " [FAILED] Error: %s", truncate(step.Result, 300))
		default:
			replanContext.WriteString(" [NOT STARTED]")
		}
		replanContext.WriteString("\n")
	}
	replanContext.WriteString("\nProduce a revised plan to accomplish the remaining work. " +
		"Do not repeat steps that were already completed successfully.")

	turnID := p.currentTurnID
	sessionID := p.currentSessionID
	// Reset plan state so the new plan.result is accepted fresh.
	p.planID = ""
	p.plan = nil
	p.currentStepIdx = 0
	p.stepHistory = nil
	p.mu.Unlock()

	_ = p.bus.Emit("plan.request", events.PlanRequest{
		TurnID:    turnID,
		SessionID: sessionID,
		Input:     replanContext.String(),
	})
}

// sendSynthesisRequest sends a final LLM request to synthesize all step results.
func (p *Plugin) sendSynthesisRequest() {
	p.mu.Lock()

	var systemPrompt strings.Builder
	if p.systemPrompt != "" {
		systemPrompt.WriteString(p.systemPrompt)
		systemPrompt.WriteString("\n\n")
	}
	if len(p.skillContexts) > 0 {
		systemPrompt.WriteString(engine.XMLWrap("skill_context", strings.Join(p.skillContexts, "\n")))
		systemPrompt.WriteString("\n")
	}

	systemPrompt.WriteString("You have completed a multi-step plan. Synthesize the results into a clear, coherent response for the user.\n\n")
	systemPrompt.WriteString(engine.XMLWrap("user_request", engine.XMLCDATA(p.originalInput)))
	systemPrompt.WriteString("\n")

	var resultsBody strings.Builder
	for i, step := range p.plan {
		content := step.Result
		if content == "" {
			content = "(no output)"
		}
		resultsBody.WriteString(engine.XMLWrap("step_result",
			engine.XMLCDATA(content),
			"number", fmt.Sprintf("%d", i+1),
			"description", step.Description,
			"status", step.Status))
	}
	systemPrompt.WriteString(engine.XMLWrap("prior_results", resultsBody.String()))
	systemPrompt.WriteString("\nProvide a comprehensive response that addresses the user's original request, ")
	systemPrompt.WriteString("incorporating the results from all completed steps. Be concise but thorough.")

	var messages []events.Message
	messages = append(messages, events.Message{
		Role:    "system",
		Content: systemPrompt.String(),
	})

	// Include the original conversation history for context.
	messages = append(messages, p.history...)

	p.mu.Unlock()

	req := events.LLMRequest{
		Role:     p.executionModelRole,
		Messages: messages,
		Stream:   true,
		Metadata: map[string]any{
			"_source": synthesizerSource,
		},
	}

	if veto, err := p.bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", req)
}

// handleSynthesizerResponse processes the final synthesis response.
func (p *Plugin) handleSynthesizerResponse(resp events.LLMResponse) {
	p.mu.Lock()

	if p.phase != phaseSynthesizing {
		p.mu.Unlock()
		return
	}

	// Add the synthesized response to main history.
	p.history = append(p.history, events.Message{
		Role:    "assistant",
		Content: resp.Content,
	})

	turnID := p.currentTurnID
	streamed := p.streamed
	p.streamed = false
	p.phase = phaseIdle
	iteration := p.iteration
	p.mu.Unlock()

	p.emitStatus("idle", "")

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

	_ = p.bus.Emit("agent.turn.end", events.TurnInfo{
		TurnID:    turnID,
		Iteration: iteration,
	})
}

// Helper methods.

// emitPlanUpdate emits an agent.plan event reflecting current plan state.
func (p *Plugin) emitPlanUpdate(turnID string) {
	p.mu.Lock()
	steps := make([]events.PlanStep, len(p.plan))
	for i, s := range p.plan {
		steps[i] = events.PlanStep{
			Description: s.Description,
			Status:      s.Status,
		}
	}
	p.mu.Unlock()

	_ = p.bus.Emit("agent.plan", events.Plan{
		Steps:  steps,
		TurnID: turnID,
	})
}

// emitStatus emits an io.status event.
func (p *Plugin) emitStatus(state, detail string) {
	_ = p.bus.Emit("io.status", events.StatusUpdate{
		State:  state,
		Detail: detail,
	})
}

// formatPlanForDisplay formats a plan as a human-readable string.
func formatPlanForDisplay(steps []planStep) string {
	var sb strings.Builder
	sb.WriteString("## Execution Plan\n\n")
	for i, step := range steps {
		fmt.Fprintf(&sb, "%d. **%s**\n", i+1, step.Description)
		if step.Instructions != "" && step.Instructions != step.Description {
			fmt.Fprintf(&sb, "   %s\n", step.Instructions)
		}
	}
	return sb.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// generateTurnID produces a unique turn identifier.
func generateTurnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("turn_%x", b)
}

// generatePlanID produces a unique plan identifier.
func generatePlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("plan_%x", b)
}
