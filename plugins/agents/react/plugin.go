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
	version    = "0.2.0"
)

// Plugin implements the ReAct (Reason + Act) agent loop.
//
// Conversation history, tool registration, and streaming display are owned
// by sibling plugins (nexus.memory.conversation, nexus.tool.catalog, and the
// IO plugins respectively); ReAct queries them per-turn via the bus instead
// of maintaining its own copies. Slash-command parsing is handled by
// nexus.control.cancel via a before:io.input veto.
//
// The plugin declares its hard dependencies via Requires() so the engine
// auto-activates them with sensible defaults when the user has not listed
// them explicitly. See docs/plugins/auto-activation.md and CLAUDE.md for
// the expansion and merge rules.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	systemPrompt     string
	systemPromptFile string
	planningEnabled  bool
	modelRole        string
	toolChoiceCfg    toolChoiceConfig
	parallelTools    bool
	maxConcurrent    int

	mu                 sync.Mutex
	skillContexts      []string
	currentTurnID      string
	currentPlan        *events.PlanResult
	currentPlanStep    int // index into currentPlan.Steps; -1 when no plan is active
	iteration          int
	pendingToolCalls   int
	cancelled          bool
	toolChoiceOverride *toolChoiceOverride
	// internalCallIDs tracks ToolCall.IDs with ParentCallID!="" — sub-calls
	// dispatched from inside another tool (run_code scripts are the canonical
	// case). Their results share the outer call's TurnID, so the agent's
	// pendingToolCalls counter would otherwise decrement on inner results
	// and short-circuit the outer turn. Conversation history filtering is
	// a separate concern owned by nexus.memory.conversation.
	internalCallIDs map[string]struct{}
	// turnCtx is cancelled on user interrupt or new turn. Workers queued
	// behind the semaphore check it before emitting tool.invoke so calls
	// that never got a slot unwind with a synthetic error.
	turnCtx    context.Context
	turnCancel context.CancelFunc
	unsubs     []func()
}

// New creates a new ReAct agent plugin.
func New() engine.Plugin {
	return &Plugin{
		internalCallIDs: make(map[string]struct{}),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

// Requires declares the sibling plugins ReAct needs to function, referenced
// by capability rather than concrete plugin ID so alternate providers can
// satisfy them (e.g. a forthcoming nexus.memory.simple for tests).
//
// "memory.history" is the source of truth for LLM-native history; ReAct
// queries it via "memory.history.query" rather than maintaining its own
// p.history. Default config gives the resolved provider a 100-message
// sliding window with persistence on — the same setup the default profile
// has used historically (applies to nexus.memory.conversation; other
// providers ignore unknown keys).
//
// "control.cancel" owns the /resume slash command and cancel turn tracking;
// without it, /resume typed by the user would land in history as a literal
// message.
//
// "tool.catalog" is the shared tool registry; ReAct queries it via
// "tool.catalog.query" to build each LLM request's tools list.
//
// Auto-activation obeys the merge rule documented in engine.Requirement:
// if the user has supplied any config for the resolved provider ID, the
// user's config wins entirely and Default is discarded.
func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{
			Capability: "memory.history",
			Default: map[string]any{
				"max_messages": 100,
				"persist":      true,
			},
		},
		{Capability: "control.cancel"},
		{Capability: "tool.catalog"},
	}
}

func (p *Plugin) Capabilities() []engine.Capability { return nil }

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

	p.toolChoiceCfg = parseToolChoiceConfig(ctx.Config)

	p.maxConcurrent = 4
	if v, ok := ctx.Config["parallel_tools"].(bool); ok {
		p.parallelTools = v
	}
	if v, ok := ctx.Config["max_concurrent"].(int); ok && v > 0 {
		p.maxConcurrent = v
	} else if v, ok := ctx.Config["max_concurrent"].(float64); ok && v > 0 {
		p.maxConcurrent = int(v)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInputEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvokeEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResultEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponseEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.loaded", p.handleSkillLoadedEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.result", p.handlePlanResultEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.active", p.handleCancelEvent,
			engine.WithPriority(20), engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.resume", p.handleResumeEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("gate.llm.retry", p.handleGateRetry,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.tool_choice", p.handleToolChoiceEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.mu.Lock()
	if p.turnCancel != nil {
		p.turnCancel()
		p.turnCancel = nil
		p.turnCtx = nil
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.input", Priority: 50},
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "llm.response", Priority: 50},
		{EventType: "skill.loaded", Priority: 50},
		{EventType: "plan.result", Priority: 50},
		{EventType: "cancel.active", Priority: 20},
		{EventType: "cancel.resume", Priority: 50},
		{EventType: "gate.llm.retry", Priority: 50},
		{EventType: "agent.tool_choice", Priority: 50},
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

// handleToolInvokeEvent flags any ToolCall with a non-empty ParentCallID as
// internal so its matching result is ignored by handleToolResult's pending
// count. Conversation-history filtering is a separate concern owned by
// nexus.memory.conversation.
func (p *Plugin) handleToolInvokeEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.ParentCallID == "" {
		return
	}
	p.mu.Lock()
	p.internalCallIDs[tc.ID] = struct{}{}
	p.mu.Unlock()
}

func (p *Plugin) handleToolResultEvent(event engine.Event[any]) {
	if result, ok := event.Payload.(events.ToolResult); ok {
		p.handleToolResult(result)
	}
}

func (p *Plugin) handleSkillLoadedEvent(event engine.Event[any]) {
	if content, ok := event.Payload.(events.SkillContent); ok {
		p.handleSkillLoaded(content)
	}
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
	if p.turnCancel != nil {
		p.turnCancel()
		p.turnCancel = nil
		p.turnCtx = nil
	}
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

// handleToolChoiceEvent processes dynamic tool choice overrides from other plugins.
func (p *Plugin) handleToolChoiceEvent(event engine.Event[any]) {
	atc, ok := event.Payload.(events.AgentToolChoice)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolChoiceOverride = &toolChoiceOverride{
		Choice: events.ToolChoice{
			Mode: atc.Mode,
			Name: atc.ToolName,
		},
		Duration: atc.Duration,
	}
	p.logger.Info("tool choice override set", "mode", atc.Mode, "name", atc.ToolName, "duration", atc.Duration)
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
	p.mu.Lock()

	// Start a new turn.
	p.currentTurnID = generateTurnID()
	p.currentPlan = nil
	p.currentPlanStep = -1
	p.iteration = 0
	p.pendingToolCalls = 0
	p.toolChoiceOverride = nil
	if p.turnCancel != nil {
		p.turnCancel()
		p.turnCancel = nil
		p.turnCtx = nil
	}

	turnID := p.currentTurnID
	p.mu.Unlock()

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

	p.iteration++

	turnID := p.currentTurnID
	iteration := p.iteration

	if len(resp.ToolCalls) > 0 {
		// Iteration limiting handled by nexus.gate.endless_loop plugin.
		p.pendingToolCalls = len(resp.ToolCalls)

		parallel := p.parallelTools && len(resp.ToolCalls) > 1
		if parallel {
			if p.turnCancel != nil {
				p.turnCancel()
			}
			p.turnCtx, p.turnCancel = context.WithCancel(context.Background())
		}
		turnCtx := p.turnCtx
		maxConc := p.maxConcurrent
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

		if !parallel {
			// Sequential dispatch: emit before:tool.invoke and tool.invoke in
			// LLM-returned order. Each tool plugin executes inline on this
			// goroutine before the next iteration.
			for i, tc := range resp.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
					args = map[string]any{}
				}

				toolCall := events.ToolCall{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: args,
					TurnID:    turnID,
					Sequence:  i,
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

		// Parallel dispatch: gates evaluate serially first (preserves gate
		// state per gate priority), then passing calls fan out into bounded
		// goroutines. Vetoed calls emit synthetic results directly.
		//
		// Unlike the v1 barrier, results land in conversation history in
		// completion order rather than LLM-returned order. Both Anthropic
		// and OpenAI tolerate out-of-order tool_result blocks as long as
		// tool_use_ids match; if a future provider rejects that, the
		// reorder would belong in nexus.memory.conversation, not here.
		type prepared struct {
			call       events.ToolCall
			vetoed     bool
			vetoReason string
		}
		prep := make([]prepared, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				args = map[string]any{}
			}
			call := events.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: args,
				TurnID:    turnID,
				Sequence:  i,
			}
			if veto, err := p.bus.EmitVetoable("before:tool.invoke", &call); err == nil && veto.Vetoed {
				p.logger.Info("tool.invoke vetoed", "tool", tc.Name, "reason", veto.Reason)
				prep[i] = prepared{call: call, vetoed: true, vetoReason: veto.Reason}
			} else {
				prep[i] = prepared{call: call}
			}
		}

		for _, pr := range prep {
			if !pr.vetoed {
				continue
			}
			_ = p.bus.Emit("tool.result", events.ToolResult{
				ID:     pr.call.ID,
				Name:   pr.call.Name,
				Error:  fmt.Sprintf("Tool call vetoed: %s", pr.vetoReason),
				TurnID: turnID,
			})
		}

		sem := make(chan struct{}, maxConc)
		for _, pr := range prep {
			if pr.vetoed {
				continue
			}
			call := pr.call
			go func() {
				select {
				case sem <- struct{}{}:
				case <-turnCtx.Done():
					_ = p.bus.Emit("tool.result", events.ToolResult{
						ID:     call.ID,
						Name:   call.Name,
						Error:  "tool dispatch cancelled",
						TurnID: turnID,
					})
					return
				}
				defer func() { <-sem }()
				if turnCtx.Err() != nil {
					_ = p.bus.Emit("tool.result", events.ToolResult{
						ID:     call.ID,
						Name:   call.Name,
						Error:  "tool dispatch cancelled",
						TurnID: turnID,
					})
					return
				}
				_ = p.bus.Emit("tool.invoke", call)
			}()
		}
		return
	}

	// No tool calls: check if there are remaining plan steps before finishing.
	if p.currentPlan != nil && p.currentPlanStep >= 0 {
		if p.currentPlanStep < len(p.currentPlan.Steps) {
			p.currentPlan.Steps[p.currentPlanStep].Status = "completed"
		}

		p.currentPlanStep++
		if p.currentPlanStep < len(p.currentPlan.Steps) {
			stepIdx := p.currentPlanStep
			p.mu.Unlock()

			p.emitPlanProgress()
			p.emitStatus("thinking", fmt.Sprintf("Plan step %d/%d", stepIdx+1, len(p.currentPlan.Steps)))
			p.sendLLMRequest()
			return
		}
		p.mu.Unlock()
		p.emitPlanProgress()
		p.mu.Lock()
	}

	p.mu.Unlock()

	p.emitStatus("idle", "")

	// Emit vetoable before:io.output. Content came from llm.response and was
	// already recorded by nexus.memory.conversation at priority 10.
	output := events.AgentOutput{
		Content: resp.Content,
		Role:    "assistant",
		TurnID:  turnID,
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

	// Drop results for internal sub-calls (e.g. run_code script dispatches)
	// so the pending count only reflects LLM-requested calls. The invoke
	// was flagged in handleToolInvokeEvent via ParentCallID.
	if _, internal := p.internalCallIDs[result.ID]; internal {
		delete(p.internalCallIDs, result.ID)
		p.mu.Unlock()
		return
	}

	// Ignore tool results from other turns (e.g. subagent tool calls).
	if result.TurnID != "" && result.TurnID != p.currentTurnID {
		p.mu.Unlock()
		return
	}

	// Conversation plugin records the result at priority 10; ReAct at 50
	// only needs to track the in-flight count so it knows when to proceed.
	p.pendingToolCalls--
	allDone := p.pendingToolCalls <= 0
	p.mu.Unlock()

	if allDone {
		p.emitStatus("thinking", "Processing tool results")
		p.sendLLMRequest()
	}
}

func (p *Plugin) handleSkillLoaded(content events.SkillContent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skillContexts = append(p.skillContexts, engine.XMLWrap("skill", content.Body, "name", content.Name))
	p.logger.Info("loaded skill context", "name", content.Name)
}

func (p *Plugin) sendLLMRequest() {
	p.mu.Lock()

	// Build system prompt with skill contexts and plan using XML boundaries.
	var sysBuilder strings.Builder
	sysBuilder.WriteString(p.systemPrompt)
	if len(p.skillContexts) > 0 {
		sysBuilder.WriteString("\n\n")
		sysBuilder.WriteString(engine.XMLWrap("skill_context", strings.Join(p.skillContexts, "\n")))
	}
	if p.currentPlan != nil && len(p.currentPlan.Steps) > 0 && p.currentPlanStep >= 0 {
		var planBody strings.Builder
		planBody.WriteString(p.currentPlan.Summary)
		planBody.WriteString("\n\nFull plan:\n")
		for i, step := range p.currentPlan.Steps {
			marker := "  "
			if i < p.currentPlanStep {
				marker = "\u2713 " // checkmark for completed
			} else if i == p.currentPlanStep {
				marker = "> " // arrow for current
			}
			fmt.Fprintf(&planBody, "%sStep %d: %s\n", marker, step.Order, step.Description)
		}
		sysBuilder.WriteString("\n\n")
		sysBuilder.WriteString(engine.XMLWrap("execution_plan", planBody.String()))

		current := p.currentPlan.Steps[p.currentPlanStep]
		instructions := current.Instructions
		if instructions == "" {
			instructions = current.Description
		}
		var taskBody strings.Builder
		fmt.Fprintf(&taskBody, "Step %d of %d\n\n", current.Order, len(p.currentPlan.Steps))
		fmt.Fprintf(&taskBody, "%s\n\n", instructions)
		taskBody.WriteString("You MUST focus exclusively on this step. Do not skip ahead or work on other steps. ")
		taskBody.WriteString("When this step is complete, respond with your results — do not call any more tools.\n")
		sysBuilder.WriteString("\n")
		sysBuilder.WriteString(engine.XMLWrap("current_task", taskBody.String()))
	}
	systemPrompt := sysBuilder.String()

	// Resolve tool choice for this iteration.
	tc := resolveToolChoice(p.toolChoiceCfg, p.iteration, &p.toolChoiceOverride)

	p.mu.Unlock()

	// Query conversation history (LLM-native format) from the memory plugin.
	// The bus dispatches synchronously, so hq.Messages is populated by the
	// time Emit returns. Nil result means no memory plugin answered —
	// treated as empty history, which is still a valid request.
	hq := &events.HistoryQuery{}
	_ = p.bus.Emit("memory.history.query", hq)

	// Query tool catalog (registered tool definitions) from the catalog
	// plugin. Same pointer-mutation pattern as HistoryQuery.
	tq := &events.ToolCatalogQuery{}
	_ = p.bus.Emit("tool.catalog.query", tq)

	var messages []events.Message
	if systemPrompt != "" {
		messages = append(messages, events.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	messages = append(messages, hq.Messages...)

	req := events.LLMRequest{
		Role:       p.modelRole,
		Messages:   messages,
		Tools:      tq.Tools,
		ToolChoice: tc,
		Stream:     true,
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
