// Package a2aremote hosts the nexus.agent.a2a_remote plugin — the OUTBOUND half
// of Nexus's Agent2Agent interoperability. Where nexus.io.a2a serves a running
// Nexus instance as an A2A agent, this plugin lets a Nexus agent CALL remote A2A
// agents: any service that speaks A2A, including another Nexus instance running
// nexus.io.a2a.
//
// # The shape
//
// Each remote listed in configuration becomes one LLM-facing tool, named
// delegate_a2a_<name> by default. From the calling agent's point of view a
// remote agent is a single tool call, exactly like the local nexus.agent.delegate
// and nexus.agent.agui_remote primitives; the transport underneath is the A2A
// wire (JSON-RPC or HTTP+JSON, plus SSE for a streaming run) spoken through
// pkg/a2a/a2aclient. Nothing here reimplements a wire concern.
//
// Remotes come from CONFIGURATION ONLY. The tool schema deliberately exposes no
// URL parameter: a model-supplied endpoint would be a server-side request
// forgery surface and an unbounded spend surface at the same time, and neither
// is worth the flexibility.
//
// # Discovery is lazy
//
// A remote's Agent Card is fetched on FIRST USE, never at boot. A remote agent
// is somebody else's process, and one that is down must not be able to fail this
// instance's startup. Until the card resolves, the tool carries the operator's
// configured description; the first successful call replaces it with a
// description built from the remote's own advertised skills, re-registered once.
//
// # Budgets
//
// A remote may be named against an AgentPosture, in which case the posture's
// timeout and recursion-depth limit bound the call, exactly as they bound a
// local delegate. Only those two dimensions cross an A2A boundary — the remote
// runs its own loop under its own token and tool-call budget — so a posture that
// sets the others is refused rather than silently half-honored.
//
// # Results
//
// A2A splits an answer between the terminal status message and the task's
// artifacts. Both are folded into the tool result under XML tag boundaries, per
// the house convention for prompt-injected content, so the calling model can
// tell the agent's summary from a tool's raw output and one artifact from the
// next. Successful outcomes are cached in a bounded LRU keyed by a content hash
// of the remote, the task and the context; failures never are, so a remote that
// was briefly down is retried rather than replayed.
//
// # Failure
//
// Every failure mode — unreachable card, refused binding, protocol error, dead
// stream, exhausted budget, a task that ended FAILED, a task parked awaiting
// input — becomes a clean tool error carrying a sentence the calling model can
// act on, alongside whatever partial output did arrive. None of them is an
// engine-level failure.
//
// All communication is over the event bus; there is no direct plugin-to-plugin
// call. The posture registry is reached through the posture.registry capability
// and the engine's plugin lookup, the same way nexus.agent.delegate reaches it.
package a2aremote

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
)

const (
	pluginID = "nexus.agent.a2a_remote"
	name     = "Remote A2A Agents"
	version  = "0.1.0"

	capPostureRegistry = "posture.registry"

	// toolClass groups these tools with the other delegation primitives in the
	// catalog, which is what progressive discovery keys off.
	toolClass = "agents"
)

// registryProvider is implemented by nexus.agent.postures. Resolving the
// registry through this interface plus a capability lookup — rather than
// importing the postures package — is what keeps this a bus-level dependency
// instead of a compile-time one.
type registryProvider interface {
	Registry() posture.Registry
}

// Plugin registers one delegate-style tool per configured remote A2A agent.
type Plugin struct {
	logger *slog.Logger
	bus    engine.EventBus

	cfg     *config
	remotes map[string]*remote // tool name -> remote

	cache *resultCache

	// postures is the live posture registry, nil when none is active. A nil
	// registry is only a problem for a remote that actually names a posture;
	// see resolveBudget.
	postures posture.Registry

	// credentials builds each remote's CredentialSource. See credentials.go.
	credentials credentialFactory

	// ctx is cancelled by Shutdown so an in-flight remote call does not outlive
	// the plugin.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks the goroutines that run remote calls, so Shutdown can wait for
	// them rather than leaving them writing to a bus that is going away.
	wg sync.WaitGroup

	// mu guards live, pendingHITL and closing. All three are written from the
	// goroutines running remote calls and read from the bus dispatch goroutine.
	mu sync.Mutex
	// live is every delegated call in flight, keyed by spawn id, so a local
	// cancellation can reach the remote tasks they own. See cancel.go.
	live map[string]*session
	// pendingHITL maps a hitl.requested id to the call waiting for its answer.
	// See hitl.go.
	pendingHITL map[string]chan events.HITLResponse
	// closing is set by Shutdown before it waits, so no goroutine is added to
	// the wait group after the wait began.
	closing bool

	unsubs []func()
}

// New returns a default-configured Plugin.
func New() engine.Plugin {
	return &Plugin{
		remotes:     map[string]*remote{},
		live:        map[string]*session{},
		pendingHITL: map[string]chan events.HITLResponse{},
		credentials: buildCredential,
	}
}

func (p *Plugin) ID() string      { return pluginID }
func (p *Plugin) Name() string    { return name }
func (p *Plugin) Version() string { return version }

func (p *Plugin) Dependencies() []string { return nil }

// Requires declares the posture registry OPTIONAL, which is the difference
// between this plugin and nexus.agent.delegate.
//
// Delegate cannot do anything without a posture: a posture name is a required
// argument of its tool. Here a posture is one way to bound a remote and the
// plugin is fully usable without one, so failing boot when no registry is
// active would refuse a configuration that works. A remote that names a posture
// with no registry present fails that CALL, with an error saying which plugin
// to activate.
func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{{Capability: capPostureRegistry, Optional: true}}
}

func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus
	p.ctx, p.cancel = context.WithCancel(context.Background())

	if p.live == nil {
		p.live = map[string]*session{}
	}
	if p.pendingHITL == nil {
		p.pendingHITL = map[string]chan events.HITLResponse{}
	}

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return err
	}
	p.cfg = cfg

	if cfg.cacheEnabled {
		p.cache = newResultCache(cfg.cacheSize)
	}

	if p.credentials == nil {
		p.credentials = buildCredential
	}
	for _, ac := range cfg.agents {
		cred, err := p.credentials(ac, p.logger)
		if err != nil {
			return fmt.Errorf("%s: agent %q: %w", pluginID, ac.name, err)
		}
		ra, err := newRemote(ac, cred)
		if err != nil {
			return err
		}
		p.remotes[ac.toolName] = ra
	}

	p.postures = resolvePostureRegistry(ctx)
	if p.postures == nil {
		for _, ac := range cfg.agents {
			if ac.posture != "" {
				p.logger.Warn("a2a_remote agent names a posture but no posture registry is active",
					"agent", ac.name, "posture", ac.posture, "capability", capPostureRegistry)
			}
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.onToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
		// The answer to a chained question, from whichever transport rendered
		// it. See hitl.go.
		p.bus.Subscribe("hitl.responded", p.onHITLResponded, engine.WithPriority(50), engine.WithSource(pluginID)),
		// A local cancellation, propagated to the remote as CancelTask. See
		// cancel.go.
		p.bus.Subscribe("cancel.active", p.onCancelActive, engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

// resolvePostureRegistry looks the registry up through the capability map. Any
// step failing means no registry, which is legal here.
func resolvePostureRegistry(ctx engine.PluginContext) posture.Registry {
	providers := ctx.Capabilities[capPostureRegistry]
	if len(providers) == 0 || ctx.LookupPlugin == nil {
		return nil
	}
	plug := ctx.LookupPlugin(providers[0])
	if plug == nil {
		return nil
	}
	rp, ok := plug.(registryProvider)
	if !ok {
		return nil
	}
	return rp.Registry()
}

// Ready publishes one tool per configured remote.
//
// The descriptions published here are the pre-discovery ones: no Agent Card has
// been fetched, deliberately. Each is replaced once, by refreshDescription, the
// first time a call to that remote resolves its card.
func (p *Plugin) Ready() error {
	for _, ac := range p.cfg.agents {
		ra := p.remotes[ac.toolName]
		desc, _ := ra.description()
		if err := p.registerTool(ra, desc); err != nil {
			return err
		}
	}
	return nil
}

// registerTool emits the tool definition for one remote.
//
// The schema carries no endpoint, URL or host parameter, and must not grow one:
// the set of reachable remotes is an operator decision, not a model decision.
func (p *Plugin) registerTool(ra *remote, description string) error {
	if err := p.bus.Emit("tool.register", events.ToolDef{
		Name:        ra.cfg.toolName,
		Description: description,
		Class:       toolClass,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Natural-language description of what the remote agent should accomplish. State the goal and the acceptance criteria; the remote agent does not see this conversation.",
				},
				"context": map[string]any{
					"type":                 "object",
					"description":          "Structured context the remote agent receives alongside the task.",
					"additionalProperties": true,
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Override this call's time budget, in seconds.",
				},
			},
			"required": []string{"task"},
		},
	}); err != nil {
		return fmt.Errorf("%s: register tool %q: %w", pluginID, ra.cfg.toolName, err)
	}
	return nil
}

// refreshDescription re-registers a remote's tool once its card has resolved
// and the card-derived description differs from what is published. The catalog
// replaces an entry registered under an existing name, so this is an update
// rather than a duplicate.
func (p *Plugin) refreshDescription(ra *remote) {
	desc, changed := ra.description()
	if !changed {
		return
	}
	if err := p.registerTool(ra, desc); err != nil {
		p.logger.Warn("a2a_remote could not refresh tool description",
			"agent", ra.cfg.name, "tool", ra.cfg.toolName, "error", err)
		return
	}
	p.logger.Info("a2a_remote refreshed tool description from agent card",
		"agent", ra.cfg.name, "tool", ra.cfg.toolName)
}

// Shutdown stops accepting calls, cancels the ones in flight and waits for
// their goroutines to finish so none of them emits onto a bus that is being
// torn down.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil

	// Every remote task still in flight is abandoned the same way a cancelled
	// turn abandons one: the process is going away, and a task left running on
	// somebody else's machine for a caller that no longer exists is the same
	// waste whether a user or a signal ended the turn.
	for _, s := range p.liveSessions() {
		s.cancelLocally("the delegating instance is shutting down")
	}

	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()

	if p.cancel != nil {
		p.cancel()
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "hitl.responded", Priority: 50},
		{EventType: "cancel.active", Priority: 50},
	}
}

// Emissions lists what this plugin puts on the bus.
//
// Three groups. The tool surface (register, the vetoable result and the result)
// and the subagent lifecycle bracket a delegation. io.output and
// subagent.iteration are a remote run's progress, republished as it arrives so a
// long delegation is not a black box to the local transports; see progress.go.
// The hitl.* trio is a remote's question travelling to a human and being
// retracted when nobody answers; see hitl.go.
//
// hitl.responded is conspicuously ABSENT, and must stay absent. This plugin asks
// the question and waits — it never answers one. Emitting a response here would
// be Nexus answering on the human's behalf, which is the exact failure the
// chained human-in-the-loop path exists to prevent, and the contract test
// asserts it never happens.
func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"before:tool.result",
		"tool.result",
		"subagent.started",
		"subagent.iteration",
		"subagent.complete",
		"io.output",
		"before:hitl.requested",
		"hitl.requested",
		"hitl.cancel",
	}
}

// onToolInvoke fires for every tool.invoke and acts only on calls naming one of
// the configured remotes.
func (p *Plugin) onToolInvoke(ev engine.Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok {
		return
	}
	ra, ok := p.remotes[tc.Name]
	if !ok {
		return
	}

	in, err := parseInvocation(tc)
	if err != nil {
		p.respond(tc, outcome{err: err.Error()})
		return
	}

	// Snapshot the caller's causation depth before leaving the dispatch
	// goroutine: the causation stack is per-goroutine, so reading it inside the
	// worker would see an empty one.
	if cc, ok := p.bus.(engine.CausationController); ok {
		in.parentDepth = cc.CurrentCausationContext().Depth
	}

	// Run off the dispatch loop: a remote call takes as long as the remote's
	// work does, and the bus dispatches synchronously.
	p.spawn(func() {
		p.respond(tc, p.runRemote(p.ctx, ra, in))
	})
}

// parseInvocation reads the tool arguments.
func parseInvocation(tc events.ToolCall) (invocation, error) {
	task, _ := tc.Arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		return invocation{}, fmt.Errorf("task is required: describe what the remote agent should accomplish")
	}
	in := invocation{task: task, parentTurn: tc.TurnID}
	if raw, ok := tc.Arguments["context"].(map[string]any); ok {
		in.contextMap = raw
	}
	if v, err := configInt(tc.Arguments["timeout_seconds"]); err == nil && v > 0 {
		in.timeout = time.Duration(v) * time.Second
	}
	return in, nil
}

// respond publishes the tool result through the vetoable gate.
func (p *Plugin) respond(tc events.ToolCall, out outcome) {
	result := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        out.output,
		Error:         out.err,
		TurnID:        tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("a2a_remote tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

// ---- Observability ----

func (p *Plugin) emitStarted(spawnID string, ra *remote, in invocation) {
	_ = p.bus.Emit("subagent.started", events.SubagentStarted{
		SchemaVersion: events.SubagentStartedVersion,
		SpawnID:       spawnID,
		Task:          in.task,
		ParentTurnID:  in.parentTurn,
	})
	p.logger.Info("a2a_remote delegating",
		"agent", ra.cfg.name, "tool", ra.cfg.toolName, "spawn_id", spawnID)
}

// emitOutput republishes a remote's own narration as local assistant output,
// under the delegated run's turn id rather than the caller's, so a transport can
// group it separately from the local turn that asked for it.
func (p *Plugin) emitOutput(content, turnID string) {
	_ = p.bus.Emit("io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       content,
		Role:          "assistant",
		TurnID:        turnID,
	})
}

// emitIteration reports one unit of remote progress against the delegated run.
func (p *Plugin) emitIteration(spawnID, parentTurn string, iteration int, content string, calls []events.ToolCallRequest) {
	_ = p.bus.Emit("subagent.iteration", events.SubagentIteration{
		SchemaVersion: events.SubagentIterationVersion,
		SpawnID:       spawnID,
		Iteration:     iteration,
		Content:       content,
		ToolCalls:     calls,
		ParentTurnID:  parentTurn,
	})
}

func (p *Plugin) emitComplete(spawnID, parentTurn string, out outcome) {
	_ = p.bus.Emit("subagent.complete", events.SubagentComplete{
		SchemaVersion: events.SubagentCompleteVersion,
		SpawnID:       spawnID,
		Result:        out.output,
		Error:         out.err,
		ParentTurnID:  parentTurn,
	})
}
