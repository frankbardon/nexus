// Package delegate hosts the nexus.agent.delegate plugin — the sub-agent
// invocation primitive exposed to the LLM as a single tool call. It binds
// the pkg/delegate.Runtime to the live posture registry (via the
// posture.registry capability) and the live tool catalog (snapshotted on
// every invocation) so the LLM can call other agents like any other tool.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
)

const (
	pluginID = "nexus.agent.delegate"
	name     = "Delegate Runtime"
	version  = "0.1.0"

	toolName           = "delegate"
	defaultMaxDepth    = 3
	defaultCacheSize   = 256
	capPostureRegistry = "posture.registry"
)

// registryProvider is implemented by the postures plugin so we can pull the
// live registry by capability lookup instead of importing the postures
// package directly (which would create a plugin-to-plugin import).
type registryProvider interface {
	Registry() posture.Registry
}

// Plugin wraps a delegate.Runtime as a Nexus plugin and registers a tool
// the LLM invokes to call sub-agents.
type Plugin struct {
	logger *slog.Logger
	bus    engine.EventBus

	runtime *delegate.Runtime

	mu             sync.Mutex
	availableTools []events.ToolDef
	unsubs         []func()

	maxDepth      int
	cacheSize     int
	registryID    string
	cacheDisabled bool
}

// New returns a default-configured Plugin.
func New() engine.Plugin {
	return &Plugin{
		maxDepth:  defaultMaxDepth,
		cacheSize: defaultCacheSize,
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return name }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{
			Capability: capPostureRegistry,
			Optional:   false,
		},
	}
}
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus

	if v, ok := ctx.Config["max_depth"].(int); ok && v > 0 {
		p.maxDepth = v
	}
	if v, ok := ctx.Config["max_depth"].(float64); ok && v > 0 {
		p.maxDepth = int(v)
	}
	if v, ok := ctx.Config["cache_size"].(int); ok && v >= 0 {
		p.cacheSize = v
	}
	if v, ok := ctx.Config["cache_size"].(float64); ok && v >= 0 {
		p.cacheSize = int(v)
	}
	if v, ok := ctx.Config["cache"].(bool); ok {
		p.cacheDisabled = !v
	}

	// Resolve the posture registry through the capability map. The first
	// active provider for posture.registry wins; the lifecycle manager has
	// already pinned this at boot.
	if providers := ctx.Capabilities[capPostureRegistry]; len(providers) > 0 {
		p.registryID = providers[0]
	}
	if p.registryID == "" {
		return fmt.Errorf("delegate: no provider for capability %s", capPostureRegistry)
	}
	regPlugin := ctx.LookupPlugin(p.registryID)
	if regPlugin == nil {
		return fmt.Errorf("delegate: plugin %q not loaded", p.registryID)
	}
	rp, ok := regPlugin.(registryProvider)
	if !ok {
		return fmt.Errorf("delegate: plugin %q does not provide Registry()", p.registryID)
	}

	p.runtime = &delegate.Runtime{
		Registry:     rp.Registry(),
		Bus:          p.bus,
		Logger:       p.logger,
		MaxDepth:     p.maxDepth,
		ToolSnapshot: p.snapshotTools,
	}
	if !p.cacheDisabled {
		p.runtime.Cache = delegate.NewMemoryCache(p.cacheSize)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.register", p.onToolRegister, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.onToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	return p.bus.Emit("tool.register", events.ToolDef{
		Name:        toolName,
		Description: "Delegate a task to a named sub-agent posture. Returns the sub-agent's final response. Use this when a task benefits from a different reasoning style, restricted tool surface, or isolated context window.",
		Class:       "agents",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"posture": map[string]any{
					"type":        "string",
					"description": "Registered AgentPosture name (see posture.registry).",
				},
				"task": map[string]any{
					"type":        "string",
					"description": "Natural-language description of what the sub-agent should accomplish.",
				},
				"context": map[string]any{
					"type":                 "object",
					"description":          "Structured context the sub-agent receives alongside the task.",
					"additionalProperties": true,
				},
				"max_tokens": map[string]any{
					"type":        "integer",
					"description": "Override the posture's default token budget for this call.",
				},
				"max_tool_calls": map[string]any{
					"type":        "integer",
					"description": "Override the posture's default tool-call budget for this call.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Override the posture's default timeout (seconds) for this call.",
				},
			},
			"required": []string{"posture", "task"},
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
		"tool.result",
		"before:tool.result",
		"llm.request",
		"before:llm.request",
		"tool.invoke",
		"before:tool.invoke",
		"delegate.start",
		"delegate.complete",
	}
}

func (p *Plugin) onToolRegister(ev engine.Event[any]) {
	td, ok := ev.Payload.(events.ToolDef)
	if !ok || td.Name == toolName {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.availableTools = append(p.availableTools, td)
}

func (p *Plugin) snapshotTools() []events.ToolDef {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]events.ToolDef, len(p.availableTools))
	copy(out, p.availableTools)
	return out
}

func (p *Plugin) onToolInvoke(ev engine.Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok || tc.Name != toolName {
		return
	}

	in, err := buildInput(tc)
	if err != nil {
		p.respondError(tc, err.Error())
		return
	}

	// Run in a goroutine so the bus dispatch loop is not blocked across
	// what may be a multi-second sub-agent run.
	go func() {
		out, _ := p.runtime.Run(context.Background(), in)
		body, jerr := json.Marshal(out)
		if jerr != nil {
			p.respondError(tc, "marshal output: "+jerr.Error())
			return
		}
		result := events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            tc.ID,
			Name:          tc.Name,
			Output:        string(body),
			TurnID:        tc.TurnID,
		}
		if out.Error != "" && (out.Status == delegate.StatusError || out.Status == delegate.StatusTimeout) {
			result.Error = out.Error
		}
		if veto, vErr := p.bus.EmitVetoable("before:tool.result", &result); vErr == nil && veto.Vetoed {
			p.logger.Info("delegate tool.result vetoed", "reason", veto.Reason)
			return
		}
		_ = p.bus.Emit("tool.result", result)
	}()
}

func (p *Plugin) respondError(tc events.ToolCall, msg string) {
	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Error:         msg,
		TurnID:        tc.TurnID,
	}
	if veto, vErr := p.bus.EmitVetoable("before:tool.result", &res); vErr == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("tool.result", res)
}

func buildInput(tc events.ToolCall) (delegate.Input, error) {
	postureName, _ := tc.Arguments["posture"].(string)
	if postureName == "" {
		return delegate.Input{}, fmt.Errorf("posture is required")
	}
	task, _ := tc.Arguments["task"].(string)
	if task == "" {
		return delegate.Input{}, fmt.Errorf("task is required")
	}
	var contextMap map[string]any
	if raw, ok := tc.Arguments["context"].(map[string]any); ok {
		contextMap = raw
	}

	in := delegate.Input{
		Posture:    postureName,
		Task:       task,
		Context:    contextMap,
		ParentTurn: tc.TurnID,
	}
	if v, ok := tc.Arguments["max_tokens"].(float64); ok && v > 0 {
		in.Overrides.MaxTokens = int(v)
	}
	if v, ok := tc.Arguments["max_tool_calls"].(float64); ok && v > 0 {
		in.Overrides.MaxToolCalls = int(v)
	}
	if v, ok := tc.Arguments["timeout_seconds"].(float64); ok && v > 0 {
		in.Overrides.Timeout = time.Duration(v * float64(time.Second))
	}
	return in, nil
}
