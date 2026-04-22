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

func (p *Plugin) ID() string                        { return p.instanceID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return []string{"nexus.agent.react"} }
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
		errResult := events.ToolResult{
			ID:     tc.ID,
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
	subResult := p.runSubagent(spawnID, task, systemPrompt, modelRole, tc.TurnID)

	toolResult := events.ToolResult{
		ID:     tc.ID,
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

	_ = p.bus.Emit("subagent.started", events.SubagentStarted{
		SpawnID:      spawnID,
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

		// Subscribe to llm.response BEFORE emitting llm.request, because the
		// bus dispatches synchronously — the response fires during Emit.
		respCh := make(chan events.LLMResponse, 1)
		unsubResp := p.bus.Subscribe("llm.response", func(event engine.Event[any]) {
			resp, ok := event.Payload.(events.LLMResponse)
			if !ok {
				return
			}
			if s, _ := resp.Metadata["_source"].(string); s == source {
				select {
				case respCh <- resp:
				default:
				}
			}
		}, engine.WithPriority(1))

		// Send LLM request tagged with our source so the parent ReAct ignores it.
		req := events.LLMRequest{
			Role:     modelRole,
			Messages: history,
			Tools:    tools,
			Stream:   false,
			Metadata: map[string]any{
				"_source": source,
			},
		}
		if veto, vErr := p.bus.EmitVetoable("before:llm.request", &req); vErr == nil && veto.Vetoed {
			logger.Info("llm.request vetoed", "reason", veto.Reason)
			break
		}
		_ = p.bus.Emit("llm.request", req)

		unsubResp()

		var resp events.LLMResponse
		select {
		case resp = <-respCh:
		default:
			return p.completeSubagent(spawnID, parentTurnID, "", "no LLM response received", iteration, totalUsage, totalCost)
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

		// Emit iteration event for observability.
		_ = p.bus.Emit("subagent.iteration", events.SubagentIteration{
			SpawnID:   spawnID,
			Iteration: iteration,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
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

	// Loop exited via break (veto or error). Return last content.
	lastContent := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && history[i].Content != "" {
			lastContent = history[i].Content
			break
		}
	}
	return p.completeSubagent(spawnID, parentTurnID, lastContent, "loop terminated", 0, totalUsage, totalCost)
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

		toolCall := events.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: args,
			TurnID:    turnID,
		}

		if veto, err := p.bus.EmitVetoable("before:tool.invoke", &toolCall); err == nil && veto.Vetoed {
			p.logger.Info("subagent tool.invoke vetoed", "tool", tc.Name, "reason", veto.Reason)
			resultCh <- events.ToolResult{
				ID:     tc.ID,
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
	complete := events.SubagentComplete{
		SpawnID:      spawnID,
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
