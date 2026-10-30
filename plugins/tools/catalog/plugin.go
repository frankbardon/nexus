package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID = "nexus.tool.catalog"
	version  = "0.2.0"

	// unknownClaimPriority runs before every other tool.invoke subscriber, so
	// the "is anyone able to serve this name?" snapshot is taken while the
	// active AG-UI run (which suspends on its client tools at priority 0) is
	// still current and still answering tool.catalog.query.
	unknownClaimPriority = -100
	// unknownSettlePriority runs after every other tool.invoke subscriber
	// (the tool-result cache at 95 is the latest today), so any handler that
	// answers synchronously has already emitted its tool.result.
	unknownSettlePriority = 1000
	// resultObservePriority only needs to see tool.result; it never mutates.
	resultObservePriority = 1
)

// Plugin maintains a live registry of tool definitions emitted on
// "tool.register" and answers synchronous "tool.catalog.query" requests so
// agent plugins don't each need to cache the same list. Registration order
// is preserved; last-write-wins when a tool with the same Name re-registers.
//
// Because it is the one place that knows every registered tool, it also owns
// the unknown-tool fallback: a tool.invoke naming a tool that is neither
// registered nor offered by any tool.catalog.query responder (e.g. the active
// AG-UI run's client tools), and that no subscriber answered synchronously, gets
// an error tool.result ("unknown tool: <name>") so the waiting agent loop
// recovers on its next iteration instead of hanging the turn.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	replay *engine.ReplayState

	mu     sync.RWMutex
	tools  []events.ToolDef
	unsubs []func()

	// unclaimed tracks tool.invoke calls (by ToolCall.ID) whose name no known
	// handler owns, while their dispatch is in flight. The value flips to true
	// when a tool.result for that ID is observed before the settle handler runs
	// — a synchronous handler the catalog does not know about (e.g. the
	// progressive-discovery "discover" meta-tool, which is injected into the
	// request rather than registered) — so no duplicate result is synthesized.
	// Entries live only for the duration of one tool.invoke dispatch.
	pendMu    sync.Mutex
	unclaimed map[string]bool
}

// New creates a new tool catalog plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return "Tool Catalog" }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as the provider of "tool.catalog" —
// the shared registry agents query via "tool.catalog.query" to build each
// LLM request's tools list.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "tool.catalog",
			Description: "Shared tool registry; agents query it to assemble each LLM request's tool list.",
		},
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.replay = ctx.Replay
	p.unclaimed = make(map[string]bool)

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.register", p.handleRegister,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.catalog.query", p.handleCatalogQuery,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleInvokeClaim,
			engine.WithPriority(unknownClaimPriority), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleInvokeSettle,
			engine.WithPriority(unknownSettlePriority), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleResultObserved,
			engine.WithPriority(resultObservePriority), engine.WithSource(pluginID)),
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
		{EventType: "tool.register", Priority: 10},
		{EventType: "tool.catalog.query", Priority: 10},
		{EventType: "tool.invoke", Priority: unknownClaimPriority},
		{EventType: "tool.invoke", Priority: unknownSettlePriority},
		{EventType: "tool.result", Priority: resultObservePriority},
	}
}

// Emissions lists what the unknown-tool fallback emits: a tool.catalog.query to
// see run-scoped tools other plugins contribute, and the synthesized error
// result (through the before:tool.result gate, like any tool's result).
func (p *Plugin) Emissions() []string {
	return []string{
		"tool.catalog.query",
		"before:tool.result",
		"tool.result",
	}
}

// handleRegister records a new tool definition. If the same Name is
// re-registered (rare, but possible during plugin reload), the existing
// entry is replaced so duplicates don't accumulate.
func (p *Plugin) handleRegister(e engine.Event[any]) {
	td, ok := e.Payload.(events.ToolDef)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.tools {
		if existing.Name == td.Name {
			p.tools[i] = td
			return
		}
	}
	p.tools = append(p.tools, td)
	p.logger.Info("tool registered", "name", td.Name, "class", td.Class)
}

// handleCatalogQuery fills the pointer payload with a snapshot of the tool
// list. Caller reads q.Tools after Emit returns.
func (p *Plugin) handleCatalogQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.ToolCatalogQuery)
	if !ok {
		return
	}
	p.mu.RLock()
	out := make([]events.ToolDef, len(p.tools))
	copy(out, p.tools)
	p.mu.RUnlock()
	q.Tools = out
}

// isRegistered reports whether name is in the registry.
func (p *Plugin) isRegistered(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, td := range p.tools {
		if td.Name == name {
			return true
		}
	}
	return false
}

// offeredByQuery reports whether any tool.catalog.query responder — this
// catalog plus run-scoped contributors such as nexus.io.agui's client tools —
// lists name. It is the bus-visible definition of "some handler serves this".
func (p *Plugin) offeredByQuery(name string) bool {
	q := &events.ToolCatalogQuery{SchemaVersion: events.ToolCatalogQueryVersion}
	if err := p.bus.Emit("tool.catalog.query", q); err != nil {
		// Fail safe: if we cannot tell, assume it is served rather than risk a
		// duplicate result for a tool someone does handle.
		return true
	}
	for _, td := range q.Tools {
		if td.Name == name {
			return true
		}
	}
	return false
}

// handleInvokeClaim runs first on every tool.invoke. A call whose name the
// registry knows, or that any tool.catalog.query responder currently offers, is
// left alone. Anything else is recorded as unclaimed for the settle handler.
//
// Calls without an ID are skipped: nothing can correlate a result to them (the
// AG-UI inbound-state scene seed is one such fire-and-forget invoke).
func (p *Plugin) handleInvokeClaim(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok || tc.ID == "" || tc.Name == "" {
		return
	}
	if p.isRegistered(tc.Name) || p.offeredByQuery(tc.Name) {
		return
	}
	p.pendMu.Lock()
	p.unclaimed[tc.ID] = false
	p.pendMu.Unlock()
}

// handleResultObserved marks an unclaimed call as answered when its result is
// emitted during the same dispatch, before the settle handler runs.
func (p *Plugin) handleResultObserved(e engine.Event[any]) {
	res, ok := e.Payload.(events.ToolResult)
	if !ok || res.ID == "" {
		return
	}
	p.pendMu.Lock()
	if _, tracked := p.unclaimed[res.ID]; tracked {
		p.unclaimed[res.ID] = true
	}
	p.pendMu.Unlock()
}

// handleInvokeSettle runs last on every tool.invoke. An unclaimed call that no
// subscriber answered gets an error tool.result so the agent loop waiting on it
// can continue; the model sees the failure next iteration like any tool error.
func (p *Plugin) handleInvokeSettle(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok || tc.ID == "" {
		return
	}
	p.pendMu.Lock()
	answered, tracked := p.unclaimed[tc.ID]
	delete(p.unclaimed, tc.ID)
	p.pendMu.Unlock()
	if !tracked || answered {
		return
	}

	// Replay: the live run recorded this very synthesized result; serve it
	// from the stash like any other tool so the replay stays deterministic.
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}

	p.logger.Warn("tool invoked with no handler; returning error result",
		"tool", tc.Name, "tool_call_id", tc.ID, "turn_id", tc.TurnID)

	result := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Error:         fmt.Sprintf("unknown tool: %s", tc.Name),
		TurnID:        tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Debug("unknown-tool result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
