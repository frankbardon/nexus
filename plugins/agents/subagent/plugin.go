package subagent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	defaultPluginID = "nexus.agent.subagent"
	pluginName      = "Subagent Manager"
	version         = "0.1.0"
	defaultToolName = "spawn_subagent"
)

// Plugin manages subagent spawning and lifecycle.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	instanceID       string // full configured ID (may include /suffix)
	toolName         string
	toolDescription  string
	defaultModelRole string
	systemPrompt     string

	mu             sync.Mutex
	availableTools []events.ToolDef
	unsubs         []func()
}

func New() engine.Plugin {
	return &Plugin{
		instanceID: defaultPluginID,
		toolName:   defaultToolName,
	}
}

func (p *Plugin) ID() string      { return p.instanceID }
func (p *Plugin) Name() string    { return pluginName }
func (p *Plugin) Version() string { return version }

// Dependencies returns no plugins. Earlier versions of this plugin
// declared a dependency on nexus.agent.react, but the runSubagent
// loop manages its own LLM + tool dispatch directly via bus
// subscriptions — react is never called into. The vestigial dep
// caused a real bug when the orchestrator (which itself depends on
// subagent) shipped a config that activated react for the orchestrator
// agent's parent thread: react's io.input handler raced the
// orchestrator's, producing duplicate plans + confused output.
//
// Deployments that legitimately want a parent ReAct alongside
// subagent workers can still include nexus.agent.react in their
// `plugins.active` list explicitly; that's now an opt-in choice,
// not a side effect of using subagent.
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	// If launched as an instance (e.g. "nexus.agent.subagent/researcher"),
	// adopt the full ID and derive a tool name from the suffix.
	if ctx.InstanceID != "" {
		p.instanceID = ctx.InstanceID
		// Derive default tool name from the suffix: "nexus.agent.subagent/researcher" -> "spawn_researcher"
		if idx := strings.LastIndexByte(ctx.InstanceID, '/'); idx >= 0 {
			p.toolName = "spawn_" + ctx.InstanceID[idx+1:]
		}
	}

	if mr, ok := ctx.Config["model_role"].(string); ok {
		p.defaultModelRole = mr
	}

	if spf, ok := ctx.Config["system_prompt_file"].(string); ok {
		spf = engine.ExpandPath(spf)
		data, err := os.ReadFile(spf)
		if err != nil {
			return fmt.Errorf("subagent: failed to read system prompt file %s: %w", spf, err)
		}
		p.systemPrompt = string(data)
	}

	if sp, ok := ctx.Config["system_prompt"].(string); ok {
		p.systemPrompt = sp
	}

	// Allow explicit override of tool name and description.
	if tn, ok := ctx.Config["tool_name"].(string); ok {
		p.toolName = tn
	}
	if td, ok := ctx.Config["tool_description"].(string); ok {
		p.toolDescription = td
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(50), engine.WithSource(p.instanceID)),
		p.bus.Subscribe("tool.register", p.handleToolRegister,
			engine.WithSource(p.instanceID)),
		// subagent.spawn: programmatic spawn path used by the
		// orchestrator agent (and any other plugin that wants to drive
		// workers without going through the LLM-facing spawn_subagent
		// tool). Symmetric to handleToolInvoke: build the same
		// runSubagent inputs out of the SubagentSpawn payload, run in
		// a goroutine so the bus dispatch loop isn't blocked, and rely
		// on runSubagent to emit subagent.complete when finished.
		// Without this subscription the orchestrator emits
		// subagent.spawn into the void and waits forever for completes
		// that never come.
		p.bus.Subscribe("subagent.spawn", p.handleSubagentSpawn,
			engine.WithPriority(50), engine.WithSource(p.instanceID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	description := p.toolDescription
	if description == "" {
		description = "Spawn an independent subagent to handle a task with its own conversation context. The subagent can use tools and will return its final result. Use this for tasks that benefit from isolated reasoning."
	}

	// Register the spawn tool.
	return p.bus.Emit("tool.register", events.ToolDef{
		Name:        p.toolName,
		Description: description,
		Class:       "agents",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The task for the subagent to accomplish",
				},
				"system_prompt": map[string]any{
					"type":        "string",
					"description": "Optional system prompt for the subagent. If omitted, uses the default.",
				},
				"model_role": map[string]any{
					"type":        "string",
					"description": "Model role to use (e.g. 'reasoning', 'balanced', 'quick'). If omitted, uses plugin default.",
				},
			},
			"required": []string{"task"},
		},
	})
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
		{EventType: "tool.register"},
		{EventType: "subagent.spawn", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"before:tool.result",
		"tool.result",
		"llm.request",
		"before:llm.request",
		"tool.invoke",
		"before:tool.invoke",
		"subagent.started",
		"subagent.iteration",
		"subagent.complete",
	}
}

// handleSubagentSpawn drives a worker from a programmatic
// subagent.spawn event, the way the orchestrator agent dispatches
// parallel workers. Mirrors the logic at the end of handleToolInvoke
// but skips the tool_use/tool_result envelope wrapping (the caller is
// not the LLM — there's no tool to surface results back through).
//
// Runs in a goroutine because runSubagent is a synchronous loop that
// can take many seconds (LLM calls, tool round-trips). The bus
// dispatch is synchronous; blocking it would freeze the whole engine
// for the worker's duration. Multiple concurrent spawns each get
// their own goroutine — runSubagent has no shared mutable state
// scoped to the plugin instance.
//
// PushCausationContext is called inside the goroutine (not on the
// caller goroutine) so the bus's per-goroutine causation stack
// reflects the worker's identity for every event the worker emits.
// Mirrors pkg/delegate/runtime.go. Without this push, events from
// runSubagent fall back to the bus-wide default — losing AgentID
// and Depth — and the causation tree blank-spots out on exactly the
// concurrent path it's built to attribute.
func (p *Plugin) handleSubagentSpawn(event engine.Event[any]) {
	spawn, ok := event.Payload.(events.SubagentSpawn)
	if !ok {
		return
	}
	go func() {
		if cc, ok := p.bus.(engine.CausationController); ok {
			pop := cc.PushCausationContext(engine.CausationContext{
				AgentID: p.subagentAgentID(spawn.SpawnID),
				Depth:   spawn.ParentDepth + 1,
			})
			defer pop()
		}
		// runSubagent emits subagent.started + subagent.iteration +
		// subagent.complete on its own; we discard the returned value
		// since the caller correlates by SpawnID via the
		// subagent.complete event, not via a return path.
		_ = p.runSubagent(spawn.SpawnID, spawn.Task, spawn.SystemPrompt, spawn.ModelRole, spawn.ParentTurnID)
	}()
}

// subagentAgentID composes the AgentID stamped on every event emitted from
// this worker's goroutine. Pattern mirrors delegate's "delegate/<posture>/<id>"
// shape: "<instanceID>/<spawnID>" (e.g. "nexus.agent.subagent/researcher/abc123").
func (p *Plugin) subagentAgentID(spawnID string) string {
	return p.instanceID + "/" + spawnID
}

// currentDepth reads the calling goroutine's causation depth so a synchronous
// subagent run can derive Depth = parent.Depth + 1. Returns 0 when the bus
// doesn't implement CausationController (e.g. in unit tests with a stub bus).
func (p *Plugin) currentDepth() int {
	if cc, ok := p.bus.(engine.CausationController); ok {
		return cc.CurrentCausationContext().Depth
	}
	return 0
}

func (p *Plugin) handleToolRegister(event engine.Event[any]) {
	td, ok := event.Payload.(events.ToolDef)
	if !ok || td.Name == p.toolName {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.availableTools = append(p.availableTools, td)
}

func (p *Plugin) handleToolInvoke(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != p.toolName {
		return
	}

	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		errResult := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
			Name:   tc.Name,
			Error:  "task is required",
			TurnID: tc.TurnID,
		}
		if veto, vErr := p.bus.EmitVetoable("before:tool.result", &errResult); vErr == nil && veto.Vetoed {
			p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
			return
		}
		_ = p.bus.Emit("tool.result", errResult)
		return
	}

	systemPrompt, _ := tc.Arguments["system_prompt"].(string)
	modelRole, _ := tc.Arguments["model_role"].(string)

	spawnID := generateSpawnID()
	subResult := func() events.SubagentComplete {
		if cc, ok := p.bus.(engine.CausationController); ok {
			pop := cc.PushCausationContext(engine.CausationContext{
				AgentID: p.subagentAgentID(spawnID),
				Depth:   p.currentDepth() + 1,
			})
			defer pop()
		}
		return p.runSubagent(spawnID, task, systemPrompt, modelRole, tc.TurnID)
	}()

	toolResult := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
		Name:   tc.Name,
		Output: subResult.Result,
		Error:  subResult.Error,
		TurnID: tc.TurnID,
	}
	if veto, vErr := p.bus.EmitVetoable("before:tool.result", &toolResult); vErr == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", toolResult)
}

// runSubagent executes an isolated agent loop and returns the result.
func (p *Plugin) runSubagent(spawnID, task, systemPrompt, modelRole, parentTurnID string) events.SubagentComplete {
	logger := p.logger.With("spawn_id", spawnID)
	logger.Info("subagent starting", "task", task)

	// Resolve defaults.
	if systemPrompt == "" {
		systemPrompt = p.systemPrompt
	}
	if modelRole == "" {
		modelRole = p.defaultModelRole
	}

	turnID := "subagent_" + spawnID
	source := "subagent." + spawnID

	_ = p.bus.Emit("subagent.started", events.SubagentStarted{SchemaVersion: events.SubagentStartedVersion, SpawnID: spawnID,
		Task:         task,
		ParentTurnID: parentTurnID,
	})

	// Build the tool set (snapshot of currently available tools).
	p.mu.Lock()
	tools := make([]events.ToolDef, len(p.availableTools))
	copy(tools, p.availableTools)
	p.mu.Unlock()

	// Initialize conversation history.
	var history []events.Message
	if systemPrompt != "" {
		history = append(history, events.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	history = append(history, events.Message{
		Role:    "user",
		Content: task,
	})

	var totalUsage events.Usage
	var totalCost float64

	// Iteration limiting now handled by nexus.gate.endless_loop plugin.
	for iteration := 0; ; iteration++ {
		logger.Info("subagent iteration", "iteration", iteration)

		req := events.LLMRequest{
			Role:     modelRole,
			Messages: history,
			Tools:    tools,
			Metadata: map[string]any{
				"_source":   source,
				"task_kind": "subagent",
			},
			Tags: map[string]string{"source_plugin": defaultPluginID},
		}
		resp, err := delegate.SyncLLM(context.Background(), p.bus, req)
		if err != nil {
			logger.Info("subagent llm error", "err", err)
			return p.completeSubagent(spawnID, parentTurnID, "", err.Error(), iteration, totalUsage, totalCost)
		}

		totalUsage.PromptTokens += resp.Usage.PromptTokens
		totalUsage.CompletionTokens += resp.Usage.CompletionTokens
		totalUsage.TotalTokens += resp.Usage.TotalTokens
		totalCost += resp.CostUSD

		// Add assistant message to history.
		history = append(history, events.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Emit iteration event for observability. ParentTurnID is set
		// so UI bridges can correlate this with the started/complete
		// events for the same worker.
		_ = p.bus.Emit("subagent.iteration", events.SubagentIteration{SchemaVersion: events.SubagentIterationVersion, SpawnID: spawnID,
			Iteration:    iteration,
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			ParentTurnID: parentTurnID,
		})

		// No tool calls means we're done.
		if len(resp.ToolCalls) == 0 {
			return p.completeSubagent(spawnID, parentTurnID, resp.Content, "", iteration+1, totalUsage, totalCost)
		}

		// Execute tool calls and collect results.
		results := p.executeToolCalls(resp.ToolCalls, turnID)
		for _, result := range results {
			content := result.Output
			if result.Error != "" {
				content = "Error: " + result.Error
			}
			history = append(history, events.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: result.ID,
			})
		}
	}
}

// executeToolCalls invokes tools and collects their results.
func (p *Plugin) executeToolCalls(toolCalls []events.ToolCallRequest, turnID string) []events.ToolResult {
	results := make([]events.ToolResult, 0, len(toolCalls))
	resultCh := make(chan events.ToolResult, len(toolCalls))
	remaining := len(toolCalls)

	// Subscribe to tool results for our turn before emitting invocations.
	unsub := p.bus.Subscribe("tool.result", func(event engine.Event[any]) {
		result, ok := event.Payload.(events.ToolResult)
		if !ok || result.TurnID != turnID {
			return
		}
		select {
		case resultCh <- result:
		default:
		}
	}, engine.WithPriority(1))

	// Emit tool invocations.
	for _, tc := range toolCalls {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			args = map[string]any{}
		}

		toolCall := events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: tc.ID,
			Name:      tc.Name,
			Arguments: args,
			TurnID:    turnID,
		}

		if veto, err := p.bus.EmitVetoable("before:tool.invoke", &toolCall); err == nil && veto.Vetoed {
			p.logger.Info("subagent tool.invoke vetoed", "tool", tc.Name, "reason", veto.Reason)
			resultCh <- events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
				Name:   tc.Name,
				Error:  fmt.Sprintf("Tool call vetoed: %s", veto.Reason),
				TurnID: turnID,
			}
			continue
		}

		_ = p.bus.Emit("tool.invoke", toolCall)
	}

	// Collect all results. Since dispatch is synchronous, results are already
	// in the channel by the time we get here.
	for range remaining {
		select {
		case r := <-resultCh:
			results = append(results, r)
		default:
			// All synchronous results collected.
		}
	}

	unsub()
	return results
}

func (p *Plugin) completeSubagent(spawnID, parentTurnID, result, errMsg string, iterations int, usage events.Usage, costUSD float64) events.SubagentComplete {
	complete := events.SubagentComplete{SchemaVersion: events.SubagentCompleteVersion, SpawnID: spawnID,
		Result:       result,
		Error:        errMsg,
		Iterations:   iterations,
		TokensUsed:   usage,
		CostUSD:      costUSD,
		ParentTurnID: parentTurnID,
	}

	_ = p.bus.Emit("subagent.complete", complete)
	p.logger.Info("subagent complete",
		"spawn_id", spawnID,
		"iterations", iterations,
		"tokens", usage.TotalTokens,
		"cost_usd", costUSD,
		"has_error", errMsg != "",
	)

	return complete
}

func generateSpawnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
