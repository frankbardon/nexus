package orchestrator

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
	pluginID   = "nexus.agent.orchestrator"
	pluginName = "Orchestrator-Worker Agent"
	version    = "0.1.0"

	decomposeSource  = "nexus.agent.orchestrator.decompose"
	synthesizeSource = "nexus.agent.orchestrator.synthesize"

	defaultMaxWorkers        = 5
	defaultMaxSubtasks       = 8
	defaultWorkerMaxIter     = 10
	defaultOrchestratorRole  = "reasoning"
	defaultWorkerRole        = "balanced"
	defaultSynthesisRole     = "balanced"
)

// phase represents the orchestrator's current lifecycle phase.
type phase int

const (
	phaseIdle phase = iota
	phaseDecomposing
	phaseDispatching
	phaseExecuting
	phaseSynthesizing
)

func (ph phase) String() string {
	switch ph {
	case phaseIdle:
		return "idle"
	case phaseDecomposing:
		return "decomposing"
	case phaseDispatching:
		return "dispatching"
	case phaseExecuting:
		return "executing"
	case phaseSynthesizing:
		return "synthesizing"
	default:
		return "unknown"
	}
}

// subtask represents a decomposed unit of work.
type subtask struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Tools       []string `json:"tools,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Status      string   `json:"status"` // "pending", "dispatched", "completed", "failed", "skipped"
	Result      string   `json:"result,omitempty"`
	Error       string   `json:"error,omitempty"`
	SpawnID     string   `json:"-"` // maps to the subagent spawn ID
}

// Plugin implements the Orchestrator-Worker agent pattern.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	maxWorkers           int
	maxSubtasks          int
	workerMaxIterations  int
	systemPrompt         string
	systemPromptFile     string
	orchestratorModelRole string
	workerModelRole      string
	synthesisModelRole   string
	failFast             bool

	mu              sync.Mutex
	phase           phase
	history         []events.Message
	registeredTools []events.ToolDef
	skillContexts   []string
	currentTurnID   string
	originalInput   string
	streamed        bool

	// Subtask tracking.
	subtasks     []subtask
	spawnToTask  map[string]int // spawnID -> subtask index
	activeCount  int            // number of currently running workers

	unsubs []func()
}

// New creates a new Orchestrator-Worker agent plugin.
func New() engine.Plugin {
	return &Plugin{
		maxWorkers:            defaultMaxWorkers,
		maxSubtasks:           defaultMaxSubtasks,
		workerMaxIterations:   defaultWorkerMaxIter,
		orchestratorModelRole: defaultOrchestratorRole,
		workerModelRole:       defaultWorkerRole,
		synthesisModelRole:    defaultSynthesisRole,
		spawnToTask:           make(map[string]int),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return []string{"nexus.agent.subagent"} }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["max_workers"].(int); ok {
		p.maxWorkers = v
	} else if v, ok := ctx.Config["max_workers"].(float64); ok {
		p.maxWorkers = int(v)
	}

	if v, ok := ctx.Config["max_subtasks"].(int); ok {
		p.maxSubtasks = v
	} else if v, ok := ctx.Config["max_subtasks"].(float64); ok {
		p.maxSubtasks = int(v)
	}

	if v, ok := ctx.Config["worker_max_iterations"].(int); ok {
		p.workerMaxIterations = v
	} else if v, ok := ctx.Config["worker_max_iterations"].(float64); ok {
		p.workerMaxIterations = int(v)
	}

	if v, ok := ctx.Config["orchestrator_model_role"].(string); ok {
		p.orchestratorModelRole = v
	}

	if v, ok := ctx.Config["worker_model_role"].(string); ok {
		p.workerModelRole = v
	}

	if v, ok := ctx.Config["synthesis_model_role"].(string); ok {
		p.synthesisModelRole = v
	}

	if v, ok := ctx.Config["fail_fast"].(bool); ok {
		p.failFast = v
	}

	if v, ok := ctx.Config["system_prompt_file"].(string); ok && v != "" {
		p.systemPromptFile = v
		data, err := os.ReadFile(v)
		if err != nil {
			return fmt.Errorf("orchestrator: failed to read system prompt file %s: %w", v, err)
		}
		p.systemPrompt = string(data)
	}

	if v, ok := ctx.Config["system_prompt"].(string); ok && v != "" {
		p.systemPrompt = v
	}

	// Register event handlers.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.input", p.handleInputEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponseEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunkEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEndEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("subagent.complete", p.handleSubagentCompleteEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.loaded", p.handleSkillLoadedEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.register", p.handleToolRegisterEvent,
			engine.WithSource(pluginID)),
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
		{EventType: "llm.response", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "subagent.complete", Priority: 50},
		{EventType: "skill.loaded", Priority: 50},
		{EventType: "tool.register"},
		{EventType: "gate.llm.retry", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"before:llm.request",
		"subagent.spawn",
		"before:io.output",
		"io.output",
		"io.output.stream",
		"io.output.stream.end",
		"io.status",
		"agent.turn.start",
		"agent.turn.end",
		"agent.plan",
	}
}

// handleGateRetry is called when a gate signals that a previously vetoed LLM
// request should be retried. Re-invokes the correct method based on current phase.
func (p *Plugin) handleGateRetry(_ engine.Event[any]) {
	p.mu.Lock()
	if p.currentTurnID == "" {
		p.mu.Unlock()
		return
	}
	currentPhase := p.phase
	p.mu.Unlock()

	switch currentPhase {
	case phaseDecomposing:
		p.logger.Info("gate.llm.retry received, re-sending decompose request")
		p.sendDecomposeRequest()
	case phaseSynthesizing:
		p.logger.Info("gate.llm.retry received, re-sending synthesize request")
		p.sendSynthesizeRequest()
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
	case decomposeSource:
		p.handleDecomposeResponse(resp)
	case synthesizeSource:
		p.handleSynthesizeResponse(resp)
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

	// Only forward stream chunks during synthesis to avoid leaking
	// decomposition output to the user.
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

func (p *Plugin) handleSubagentCompleteEvent(event engine.Event[any]) {
	if complete, ok := event.Payload.(events.SubagentComplete); ok {
		p.handleSubagentComplete(complete)
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

// Core logic — Phase 1: Decomposition.

func (p *Plugin) handleInput(input events.UserInput) {
	p.mu.Lock()

	// Start a new turn.
	p.currentTurnID = generateTurnID()
	p.originalInput = input.Content
	p.streamed = false
	p.subtasks = nil
	p.spawnToTask = make(map[string]int)
	p.activeCount = 0
	p.phase = phaseDecomposing

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

	p.emitStatus("thinking", "Decomposing task into subtasks")
	p.sendDecomposeRequest()
}

// sendDecomposeRequest sends an LLM request to decompose the user's task.
func (p *Plugin) sendDecomposeRequest() {
	p.mu.Lock()

	// Build tool names list for the decomposition prompt.
	toolNames := make([]string, len(p.registeredTools))
	for i, t := range p.registeredTools {
		toolNames[i] = t.Name
	}

	// Construct system prompt with XML boundaries.
	var systemPrompt strings.Builder
	if p.systemPrompt != "" {
		systemPrompt.WriteString(p.systemPrompt)
		systemPrompt.WriteString("\n\n")
	}
	if len(p.skillContexts) > 0 {
		systemPrompt.WriteString(engine.XMLWrap("skill_context", strings.Join(p.skillContexts, "\n")))
		systemPrompt.WriteString("\n")
	}

	systemPrompt.WriteString("You are a task decomposition agent. Your role is to break down a user's request " +
		"into independent subtasks that can be executed in parallel by worker agents.\n\n" +
		"Each subtask will be handled by an isolated worker agent with its own conversation context. " +
		"Workers can use tools but cannot communicate with each other directly. " +
		"Use depends_on to express ordering constraints between subtasks.\n\n" +
		"Guidelines:\n" +
		"- Keep subtasks focused and atomic.\n" +
		"- Maximize parallelism: only add depends_on when a subtask truly needs another's output.\n" +
		"- Each subtask description should be self-contained enough for a worker to execute independently.\n" +
		"- Specify which tools each subtask needs (empty means all available tools).\n")

	var messages []events.Message
	messages = append(messages, events.Message{
		Role:    "system",
		Content: systemPrompt.String(),
	})

	// Include conversation history for context.
	messages = append(messages, p.history...)

	maxSubtasks := p.maxSubtasks
	p.mu.Unlock()

	// Append decomposition instruction.
	decomposeInstruction := fmt.Sprintf(
		"Decompose the user's request into subtasks. Available tools: %s\n\n"+
			"Respond with ONLY a JSON object in this exact format:\n"+
			"```json\n"+
			"{\n"+
			"  \"subtasks\": [\n"+
			"    {\n"+
			"      \"id\": \"task_1\",\n"+
			"      \"description\": \"What this subtask accomplishes — include all necessary context\",\n"+
			"      \"tools\": [\"tool_name\"],\n"+
			"      \"depends_on\": []\n"+
			"    }\n"+
			"  ]\n"+
			"}\n"+
			"```\n"+
			"Each subtask ID must be unique. depends_on references other subtask IDs. "+
			"No more than %d subtasks.",
		strings.Join(toolNames, ", "),
		maxSubtasks,
	)

	messages = append(messages, events.Message{
		Role:    "user",
		Content: decomposeInstruction,
	})

	req := events.LLMRequest{
		Role:     p.orchestratorModelRole,
		Messages: messages,
		Stream:   false, // Need full response to parse JSON.
		Metadata: map[string]any{
			"_source": decomposeSource,
		},
	}

	if veto, err := p.bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", req)
}

// handleDecomposeResponse processes the LLM's decomposition response.
func (p *Plugin) handleDecomposeResponse(resp events.LLMResponse) {
	p.mu.Lock()
	if p.phase != phaseDecomposing {
		p.mu.Unlock()
		return
	}
	turnID := p.currentTurnID
	p.mu.Unlock()

	tasks, err := parseSubtasksJSON(resp.Content)
	if err != nil {
		p.logger.Error("failed to parse subtasks from LLM response", "error", err)
		// Fall back: treat the entire task as a single subtask.
		tasks = []subtask{
			{
				ID:          "task_1",
				Description: p.originalInput,
				Status:      "pending",
			},
		}
	}

	p.mu.Lock()

	// Enforce max subtasks.
	if len(tasks) > p.maxSubtasks {
		tasks = tasks[:p.maxSubtasks]
	}

	// Normalize subtask state.
	for i := range tasks {
		tasks[i].Status = "pending"
		if tasks[i].ID == "" {
			tasks[i].ID = fmt.Sprintf("task_%d", i+1)
		}
	}

	// Validate dependency references — strip any that reference nonexistent IDs.
	validIDs := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		validIDs[t.ID] = true
	}
	for i := range tasks {
		var validDeps []string
		for _, dep := range tasks[i].DependsOn {
			if validIDs[dep] {
				validDeps = append(validDeps, dep)
			}
		}
		tasks[i].DependsOn = validDeps
	}

	p.subtasks = tasks
	p.phase = phaseDispatching
	p.mu.Unlock()

	p.logger.Info("task decomposed", "subtasks", len(tasks), "turn", turnID)

	// Emit plan for UI display.
	p.emitPlanUpdate(turnID)

	// Begin dispatching workers.
	p.dispatchReadyWorkers()
}

// Core logic — Phase 2: Worker Dispatch.

// dispatchReadyWorkers identifies subtasks whose dependencies are satisfied and spawns workers.
func (p *Plugin) dispatchReadyWorkers() {
	p.mu.Lock()

	// Build completed set for dependency checking.
	completed := make(map[string]bool, len(p.subtasks))
	failed := make(map[string]bool, len(p.subtasks))
	for _, t := range p.subtasks {
		switch t.Status {
		case "completed":
			completed[t.ID] = true
		case "failed":
			failed[t.ID] = true
		}
	}

	// Collect subtasks ready to dispatch.
	var toDispatch []int
	for i := range p.subtasks {
		if p.subtasks[i].Status != "pending" {
			continue
		}
		if p.activeCount+len(toDispatch) >= p.maxWorkers {
			break
		}

		// Check if all dependencies are met.
		ready := true
		blocked := false
		for _, dep := range p.subtasks[i].DependsOn {
			if failed[dep] {
				blocked = true
				break
			}
			if !completed[dep] {
				ready = false
				break
			}
		}

		if blocked {
			// A dependency failed — skip this subtask.
			p.subtasks[i].Status = "skipped"
			p.subtasks[i].Error = "dependency failed"
			continue
		}

		if ready {
			toDispatch = append(toDispatch, i)
		}
	}

	// Update phase based on what we found.
	if len(toDispatch) == 0 && p.activeCount == 0 {
		// Nothing left to dispatch or wait for — move to synthesis.
		p.phase = phaseSynthesizing
		turnID := p.currentTurnID
		p.mu.Unlock()
		p.emitPlanUpdate(turnID)
		p.emitStatus("thinking", "All workers complete, synthesizing results")
		p.sendSynthesizeRequest()
		return
	}

	if len(toDispatch) > 0 {
		p.phase = phaseExecuting
	}

	// Build spawn data while holding the lock.
	type spawnInfo struct {
		spawnID     string
		task        string
		tools       []string
		idx         int
		depResults  string
	}

	spawns := make([]spawnInfo, len(toDispatch))
	for si, idx := range toDispatch {
		spawnID := generateSpawnID()
		p.subtasks[idx].Status = "dispatched"
		p.subtasks[idx].SpawnID = spawnID
		p.spawnToTask[spawnID] = idx
		p.activeCount++

		// Gather results from dependencies to include in worker context.
		var depResults strings.Builder
		for _, dep := range p.subtasks[idx].DependsOn {
			for _, t := range p.subtasks {
				if t.ID == dep && t.Result != "" {
					depResults.WriteString(engine.XMLWrap("dependency_result",
						engine.XMLCDATA(t.Result),
						"task_id", t.ID))
				}
			}
		}

		spawns[si] = spawnInfo{
			spawnID:    spawnID,
			task:       p.subtasks[idx].Description,
			tools:      p.subtasks[idx].Tools,
			idx:        idx,
			depResults: depResults.String(),
		}
	}

	turnID := p.currentTurnID
	workerRole := p.workerModelRole
	p.mu.Unlock()

	// Emit updated plan.
	p.emitPlanUpdate(turnID)

	p.emitStatus("thinking", fmt.Sprintf("Dispatching %d worker(s)", len(spawns)))

	// Spawn workers. The subagent plugin handles these events synchronously,
	// so each spawn blocks until the subagent completes. We emit them and rely
	// on subagent.complete events to track progress.
	for _, s := range spawns {
		// Build a focused system prompt for the worker with XML boundaries.
		var workerPrompt strings.Builder
		workerPrompt.WriteString("You are a focused worker agent. Complete the assigned task thoroughly and concisely.\n\n")
		workerPrompt.WriteString(engine.XMLWrap("current_task", s.task))
		if s.depResults != "" {
			workerPrompt.WriteString("\n")
			workerPrompt.WriteString(engine.XMLWrap("prior_results", s.depResults))
		}
		workerPrompt.WriteString("\nWhen finished, provide a clear summary of what was accomplished and any relevant results.")

		p.logger.Info("dispatching worker",
			"spawn_id", s.spawnID,
			"task_id", p.subtasks[s.idx].ID,
			"task", s.task,
		)

		_ = p.bus.Emit("subagent.spawn", events.SubagentSpawn{
			SpawnID:      s.spawnID,
			Task:         s.task,
			SystemPrompt: workerPrompt.String(),
			Tools:        s.tools,
			ModelRole:    workerRole,
			ParentTurnID: turnID,
		})
	}
}

// handleSubagentComplete processes a worker completion event.
func (p *Plugin) handleSubagentComplete(complete events.SubagentComplete) {
	p.mu.Lock()

	idx, ok := p.spawnToTask[complete.SpawnID]
	if !ok {
		// Not one of our workers.
		p.mu.Unlock()
		return
	}

	delete(p.spawnToTask, complete.SpawnID)

	if complete.Error != "" {
		p.subtasks[idx].Status = "failed"
		p.subtasks[idx].Error = complete.Error
		p.subtasks[idx].Result = complete.Result
		p.logger.Warn("worker failed",
			"spawn_id", complete.SpawnID,
			"task_id", p.subtasks[idx].ID,
			"error", complete.Error,
			"iterations", complete.Iterations,
		)
	} else {
		p.subtasks[idx].Status = "completed"
		p.subtasks[idx].Result = complete.Result
		p.logger.Info("worker completed",
			"spawn_id", complete.SpawnID,
			"task_id", p.subtasks[idx].ID,
			"iterations", complete.Iterations,
			"tokens", complete.TokensUsed.TotalTokens,
		)
	}

	p.activeCount--
	turnID := p.currentTurnID
	shouldFailFast := p.failFast && complete.Error != ""

	p.mu.Unlock()

	// Emit updated plan.
	p.emitPlanUpdate(turnID)

	if shouldFailFast {
		p.logger.Warn("fail_fast enabled, cancelling remaining workers")
		p.cancelRemainingSubtasks()
		p.mu.Lock()
		p.phase = phaseSynthesizing
		p.mu.Unlock()
		p.emitStatus("thinking", "Worker failed, synthesizing partial results")
		p.sendSynthesizeRequest()
		return
	}

	// Try to dispatch more workers now that a dependency may have been satisfied.
	p.dispatchReadyWorkers()
}

// cancelRemainingSubtasks marks all pending/dispatched subtasks as skipped.
func (p *Plugin) cancelRemainingSubtasks() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.subtasks {
		switch p.subtasks[i].Status {
		case "pending", "dispatched":
			p.subtasks[i].Status = "skipped"
			p.subtasks[i].Error = "cancelled due to fail_fast"
		}
	}
}

// Core logic — Phase 3: Synthesis.

// sendSynthesizeRequest sends a streaming LLM request to combine all worker results.
func (p *Plugin) sendSynthesizeRequest() {
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

	systemPrompt.WriteString("Multiple worker agents have completed subtasks in parallel. " +
		"Synthesize their results into a clear, coherent response for the user.\n\n")
	systemPrompt.WriteString(engine.XMLWrap("user_request", engine.XMLCDATA(p.originalInput)))
	systemPrompt.WriteString("\n")

	var resultsBody strings.Builder
	for i, t := range p.subtasks {
		var content string
		switch t.Status {
		case "completed":
			if t.Result != "" {
				content = t.Result
			} else {
				content = "(no output)"
			}
		case "failed":
			content = "Failed: " + t.Error
			if t.Result != "" {
				content += "\nPartial result: " + t.Result
			}
		case "skipped":
			content = "Skipped: " + t.Error
		default:
			content = "(no output)"
		}
		resultsBody.WriteString(engine.XMLWrap("subtask_result",
			engine.XMLCDATA(content),
			"number", fmt.Sprintf("%d", i+1),
			"description", t.Description,
			"status", t.Status))
	}
	systemPrompt.WriteString(engine.XMLWrap("subtask_results", resultsBody.String()))
	systemPrompt.WriteString("\nProvide a comprehensive response that addresses the user's original request, ")
	systemPrompt.WriteString("incorporating the results from all completed subtasks. ")
	systemPrompt.WriteString("If any subtasks failed or were skipped, note what could not be accomplished and why. ")
	systemPrompt.WriteString("Be concise but thorough.")

	var messages []events.Message
	messages = append(messages, events.Message{
		Role:    "system",
		Content: systemPrompt.String(),
	})

	// Include conversation history for context.
	messages = append(messages, p.history...)

	synthesisRole := p.synthesisModelRole
	p.mu.Unlock()

	req := events.LLMRequest{
		Role:     synthesisRole,
		Messages: messages,
		Stream:   true,
		Metadata: map[string]any{
			"_source": synthesizeSource,
		},
	}

	if veto, err := p.bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", req)
}

// handleSynthesizeResponse processes the final synthesis LLM response.
func (p *Plugin) handleSynthesizeResponse(resp events.LLMResponse) {
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
		TurnID: turnID,
	})
}

// Helper methods.

// emitPlanUpdate emits an agent.plan event reflecting current subtask state.
func (p *Plugin) emitPlanUpdate(turnID string) {
	p.mu.Lock()
	steps := make([]events.PlanStep, len(p.subtasks))
	for i, t := range p.subtasks {
		status := t.Status
		// Map "dispatched" to "active" for UI display.
		if status == "dispatched" {
			status = "active"
		}
		steps[i] = events.PlanStep{
			Description: t.Description,
			Status:      status,
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

// parseSubtasksJSON extracts subtasks from the LLM's JSON response.
func parseSubtasksJSON(content string) ([]subtask, error) {
	// Try to extract JSON from markdown code blocks first.
	jsonStr := content
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			jsonStr = content[start : start+end]
		}
	} else if idx := strings.Index(content, "```"); idx >= 0 {
		start := idx + len("```")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			jsonStr = content[start : start+end]
		}
	}

	jsonStr = strings.TrimSpace(jsonStr)

	// Try parsing as an object with a "subtasks" array.
	var subtasksObj struct {
		Subtasks []subtask `json:"subtasks"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &subtasksObj); err == nil && len(subtasksObj.Subtasks) > 0 {
		return subtasksObj.Subtasks, nil
	}

	// Try parsing as a raw array of subtasks.
	var tasks []subtask
	if err := json.Unmarshal([]byte(jsonStr), &tasks); err == nil && len(tasks) > 0 {
		return tasks, nil
	}

	return nil, fmt.Errorf("could not parse subtasks JSON from response: %.200s", content)
}

// generateTurnID produces a unique turn identifier.
func generateTurnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("turn_%x", b)
}

// generateSpawnID produces a unique spawn identifier.
func generateSpawnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("orch_%x", b)
}
