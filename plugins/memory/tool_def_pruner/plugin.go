// Package tool_def_pruner removes tool definitions from outgoing
// LLMRequest.Tools when those tools have been idle past a turn threshold.
// Pairs with nexus.discovery.progressive: progressive scopes by class;
// this plugin scopes per individual tool.
//
// Runs at priority 14 on before:llm.request — after both
// progressive (8) and tool_result_clear (12) have shaped the request.
// Idle tools are surfaced as MemoryToolDefPruned events; they reappear
// automatically next turn if the agent invokes them via discover or any
// other path, since this plugin observes tool.invoke and resets the
// last-used counter.
package tool_def_pruner

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.memory.tool_def_pruner"
	pluginName = "Tool-Definition Pruner"
	version    = "0.1.0"
)

// Plugin tracks per-tool last-used turn and prunes idle definitions.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	enabled              bool
	unusedTurnsThreshold int
	// neverPrune lists tool names that should always remain visible. The
	// agent's "discover" meta-tool is added by default so progressive
	// discovery isn't accidentally severed when nothing has been invoked
	// yet.
	neverPrune map[string]bool

	mu       sync.Mutex
	turn     int
	lastUsed map[string]int // tool_name -> turn
	pruned   map[string]bool

	unsubs []func()
}

// New creates a new tool_def_pruner plugin.
func New() engine.Plugin {
	return &Plugin{
		enabled:              true,
		unusedTurnsThreshold: 6,
		neverPrune: map[string]bool{
			"discover": true,
			"ask_user": true,
		},
		lastUsed: make(map[string]int),
		pruned:   make(map[string]bool),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 60},
		{EventType: "agent.turn.end", Priority: 60},
		{EventType: "before:llm.request", Priority: 14},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"memory.tool_def_pruned",
		"memory.curated",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["enabled"].(bool); ok {
		p.enabled = v
	}
	if v, ok := ctx.Config["unused_turns_threshold"].(int); ok {
		p.unusedTurnsThreshold = v
	} else if v, ok := ctx.Config["unused_turns_threshold"].(float64); ok {
		p.unusedTurnsThreshold = int(v)
	}
	if list, ok := ctx.Config["never_prune"].([]any); ok {
		// Replace defaults entirely when the operator supplies a list —
		// matches the principle of letting users opt out of "discover"
		// being kept if they have a different agent shape.
		p.neverPrune = make(map[string]bool, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				p.neverPrune[s] = true
			}
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd,
			engine.WithPriority(60), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(14), engine.WithSource(pluginID)),
	)

	p.logger.Info("tool_def_pruner initialized",
		"enabled", p.enabled,
		"unused_turns_threshold", p.unusedTurnsThreshold,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	return nil
}

// handleToolInvoke marks a tool as "just used" — resets idle counter.
func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	tc, ok := e.Payload.(events.ToolCall)
	if !ok || tc.Name == "" {
		return
	}
	p.mu.Lock()
	p.lastUsed[tc.Name] = p.turn
	delete(p.pruned, tc.Name)
	p.mu.Unlock()
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	if _, ok := e.Payload.(events.TurnInfo); !ok {
		return
	}
	p.mu.Lock()
	p.turn++
	p.mu.Unlock()
}

// handleBeforeLLMRequest filters req.Tools, dropping definitions for tools
// that have been idle past the threshold. Tools never seen are tracked
// with first-seen turn so we don't prune freshly-loaded tools instantly.
func (p *Plugin) handleBeforeLLMRequest(e engine.Event[any]) {
	if !p.enabled {
		return
	}
	vp, ok := e.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}
	if len(req.Tools) == 0 {
		return
	}

	p.mu.Lock()
	now := p.turn
	keep := make([]events.ToolDef, 0, len(req.Tools))
	pruned := make([]events.MemoryToolDefPruned, 0)
	sections := make([]events.CurationSection, 0)

	for _, td := range req.Tools {
		if p.neverPrune[td.Name] {
			keep = append(keep, td)
			continue
		}
		last, seen := p.lastUsed[td.Name]
		if !seen {
			// First sight — register as just-loaded so the threshold
			// counts from this turn, not from turn zero.
			p.lastUsed[td.Name] = now
			keep = append(keep, td)
			continue
		}
		if now-last < p.unusedTurnsThreshold {
			keep = append(keep, td)
			continue
		}

		size := defSize(td)
		pruned = append(pruned, events.MemoryToolDefPruned{
			SchemaVersion:  events.MemoryToolDefPrunedVersion,
			ToolID:         td.Name,
			LastUsedTurn:   last,
			DefinitionSize: size,
		})
		sections = append(sections, events.CurationSection{
			SectionID:   pluginID + "/" + td.Name,
			Kind:        "session",
			TokensDelta: -size / 4,
		})
		p.pruned[td.Name] = true
	}
	p.mu.Unlock()

	if len(pruned) == 0 {
		return
	}
	req.Tools = keep
	for _, ev := range pruned {
		_ = p.bus.Emit("memory.tool_def_pruned", ev)
	}
	_ = p.bus.Emit("memory.curated", events.MemoryCurated{
		SchemaVersion:    events.MemoryCuratedVersion,
		Layer:            "tool_def_pruner",
		SectionsTouched:  sections,
		CacheInvalidates: true,
		AtTurn:           now,
	})
}

// defSize estimates the byte size of a tool definition for observability.
func defSize(td events.ToolDef) int {
	raw, _ := json.Marshal(td)
	return len(raw)
}
