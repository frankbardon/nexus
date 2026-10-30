// Package tooltimeout implements nexus.gate.tool_timeout, a per-call deadline
// gate for tool invocations. It observes tool.invoke, starts a timer per
// call, and on expiry synthesizes a tool.result error plus a tool.timeout
// event so the agent's bookkeeping unwinds even when a tool hangs.
//
// The gate is on by default: nexus.tool.catalog auto-activates it through
// Requires(), so every profile that dispatches tools through the catalog gets
// a per-call deadline without listing the gate. The default deadline is a
// backstop (minutes, not seconds) sized above every shipped tool's own
// timeout, and tools whose wait is bounded by a human or by their own
// wall-clock budget are exempt by default (see builtinPerTool).
package tooltimeout

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.tool_timeout"

// defaultTimeout is the backstop deadline for any tool with no per_tool entry.
// It is deliberately far above the internal timeouts of the shipped tools
// (shell 30s, run_code 30s, web 20s, MCP 30s, delegate_agui_* 120s,
// delegate_a2a_* 5m per call): its job is to end a turn whose tool result
// never arrives (a vetoed before:tool.result, a handler that never answers),
// not to police a tool that is merely slow.
const defaultTimeout = 10 * time.Minute

// builtinPerTool exempts (0 = no deadline) the tools whose wait is either
// bounded by a human or by a wall-clock budget the tool enforces itself:
//
//   - ask_user waits on a human answer, which has no machine upper bound.
//   - delegate, spawn_* (subagent instances), delegate_a2a_* and
//     delegate_agui_* run a whole nested agent under their own posture or
//     per-remote timeout, and may park on HITL; a shorter outer deadline
//     would cut them off below what the operator configured.
//
// Any user per_tool key (exact or glob) is consulted before these, so an
// operator can still bound one of them, e.g. per_tool: {ask_user: 15m}.
var builtinPerTool = map[string]time.Duration{
	"ask_user":        0,
	"delegate":        0,
	"spawn_*":         0,
	"delegate_a2a_*":  0,
	"delegate_agui_*": 0,
}

// New creates a new tool_timeout gate plugin instance.
func New() engine.Plugin {
	return &Plugin{
		defaultTimeout: defaultTimeout,
		perTool:        map[string]time.Duration{},
		builtin:        builtinPerTool,
		inflight:       map[string]*tracker{},
		timedOut:       map[string]struct{}{},
		synthesizing:   map[string]struct{}{},
	}
}

// Plugin watches tool.invoke / tool.result and synthesizes a tool.result
// error + tool.timeout event when a call exceeds its deadline. It does not
// veto before:tool.invoke; other gates remain free to refuse the call.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	defaultTimeout time.Duration
	// perTool keys are either exact tool names or path.Match-style globs.
	// Resolution: exact key beats glob; among glob matches the longest
	// pattern wins. Documented in schema.json.
	perTool map[string]time.Duration
	// builtin holds the default exemptions, consulted after perTool. Nil in
	// hand-built test plugins, which then see only perTool.
	builtin map[string]time.Duration
	// disabled turns every deadline off (enabled: false).
	disabled bool

	mu       sync.Mutex
	inflight map[string]*tracker // call ID -> live timer
	timedOut map[string]struct{} // call IDs we've already synthesized for; veto late real results
	// synthesizing tracks IDs whose synthetic tool.result is currently in
	// flight on the bus. Used to skip cleanup in handleToolResult so
	// timedOut survives long enough for the eventual real result to be
	// vetoed in handleBeforeToolResult.
	synthesizing map[string]struct{}

	unsubs []func()
}

type tracker struct {
	cancel context.CancelFunc
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Tool Timeout Gate" }
func (p *Plugin) Version() string                   { return "0.2.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["enabled"].(bool); ok && !v {
		p.disabled = true
	}

	if v, ok := ctx.Config["default_timeout"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("default_timeout %q: %w", v, err)
		}
		if d < 0 {
			return fmt.Errorf("default_timeout %q must not be negative (0 means no default deadline)", v)
		}
		p.defaultTimeout = d
	}

	if v, ok := ctx.Config["per_tool"].(map[string]any); ok {
		for name, raw := range v {
			s, ok := raw.(string)
			if !ok || s == "" {
				return fmt.Errorf("per_tool[%q]: expected duration string, got %T", name, raw)
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("per_tool[%q] %q: %w", name, s, err)
			}
			if d < 0 {
				return fmt.Errorf("per_tool[%q] %q must not be negative (0 means no deadline)", name, s)
			}
			p.perTool[name] = d
		}
	}

	// Start timer on tool.invoke (post-veto) so vetoed calls don't leave
	// orphaned timers. Subscribe to tool.result to cancel; before:tool.result
	// suppresses any late real result for an ID we've already synthesized.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult,
			engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:tool.result", p.handleBeforeToolResult,
			engine.WithPriority(5), engine.WithSource(pluginID)),
	)

	p.logger.Info("tool_timeout gate initialized",
		"enabled", !p.disabled,
		"default_timeout", p.defaultTimeout,
		"per_tool_overrides", len(p.perTool),
		"builtin_exemptions", len(p.builtin))
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.mu.Lock()
	for id, t := range p.inflight {
		t.cancel()
		delete(p.inflight, id)
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 10},
		{EventType: "tool.result", Priority: 5},
		{EventType: "before:tool.result", Priority: 5},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"tool.result", "tool.timeout"}
}

func (p *Plugin) handleToolInvoke(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.ID == "" {
		return
	}

	timeout := p.resolveTimeout(tc.Name)
	if timeout <= 0 {
		return
	}

	// Cooperative cancellation: this context is plumbed nowhere by the
	// engine today — tools that already use exec.CommandContext or net/http
	// with their own timeout honor their own deadline. Our timer's role is
	// purely to unblock the agent's bookkeeping (pendingToolCalls,
	// semaphore) by synthesizing a tool.result on expiry. The original tool
	// goroutine may keep running until it completes or the process exits.
	timerCtx, cancel := context.WithTimeout(context.Background(), timeout)

	p.mu.Lock()
	// Defensive: replace any prior tracker for the same ID. A repeated ID
	// would be an upstream bug, but cancelling the older timer is correct.
	if prev, ok := p.inflight[tc.ID]; ok {
		prev.cancel()
	}
	p.inflight[tc.ID] = &tracker{cancel: cancel}
	p.mu.Unlock()

	go p.watch(timerCtx, tc, timeout)
}

func (p *Plugin) watch(ctx context.Context, tc events.ToolCall, timeout time.Duration) {
	<-ctx.Done()
	if ctx.Err() != context.DeadlineExceeded {
		// Cancelled because tool.result arrived first.
		return
	}

	p.mu.Lock()
	if _, ok := p.inflight[tc.ID]; !ok {
		// Result already arrived between deadline fire and lock acquire.
		p.mu.Unlock()
		return
	}
	delete(p.inflight, tc.ID)
	p.timedOut[tc.ID] = struct{}{}
	p.synthesizing[tc.ID] = struct{}{}
	p.mu.Unlock()

	override := fmt.Sprintf("gates.tool_timeout.per_tool.%s", tc.Name)
	guidance := fmt.Sprintf("tool %s exceeded timeout %s; raise via %s: <duration>",
		tc.Name, timeout, override)

	p.logger.Warn("tool timeout fired",
		"tool", tc.Name,
		"call_id", tc.ID,
		"timeout", timeout,
		"override", override)

	_ = p.bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion, ToolName: tc.Name,
		CallID:   tc.ID,
		Timeout:  timeout,
		Override: override,
		TurnID:   tc.TurnID,
	})

	_ = p.bus.Emit("tool.result", events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
		Name:   tc.Name,
		Error:  guidance,
		TurnID: tc.TurnID,
	})

	// Synthetic emission complete — drop the marker. timedOut stays so
	// any late real tool.result for this ID is vetoed in
	// handleBeforeToolResult.
	p.mu.Lock()
	delete(p.synthesizing, tc.ID)
	p.mu.Unlock()
}

func (p *Plugin) handleToolResult(event engine.Event[any]) {
	res, ok := event.Payload.(events.ToolResult)
	if !ok || res.ID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.inflight[res.ID]; ok {
		t.cancel()
		delete(p.inflight, res.ID)
	}
	if _, mid := p.synthesizing[res.ID]; mid {
		// Our own synthesized result echoing through the bus — preserve
		// timedOut so the eventual real result is suppressed.
		return
	}
	// Real tool.result (post-synthesis or never-timed-out) — clear the
	// marker so memory doesn't grow unbounded.
	delete(p.timedOut, res.ID)
}

// handleBeforeToolResult vetoes a real tool.result that arrives after we've
// already synthesized one for the same call. Without this, the agent's
// pendingToolCalls counter would decrement twice and break turn accounting.
func (p *Plugin) handleBeforeToolResult(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	res, ok := vp.Original.(*events.ToolResult)
	if !ok || res.ID == "" {
		return
	}
	p.mu.Lock()
	_, suppress := p.timedOut[res.ID]
	p.mu.Unlock()
	if suppress {
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: fmt.Sprintf("tool %s already timed out; suppressing late result", res.Name),
		}
	}
}

// resolveTimeout returns the timeout for a given tool name; 0 means no
// deadline. Resolution: the user's per_tool map (exact key, then longest
// matching glob), then the built-in exemptions (same order), then
// default_timeout. enabled: false short-circuits to 0.
func (p *Plugin) resolveTimeout(name string) time.Duration {
	if p.disabled {
		return 0
	}
	if d, ok := lookup(p.perTool, name); ok {
		return d
	}
	if d, ok := lookup(p.builtin, name); ok {
		return d
	}
	return p.defaultTimeout
}

// lookup resolves name against one override map: an exact key wins; among
// path.Match glob matches the longest pattern wins.
func lookup(m map[string]time.Duration, name string) (time.Duration, bool) {
	if d, ok := m[name]; ok {
		return d, true
	}
	// Walk keys in deterministic (longest first, then lexical) order so glob
	// fall-backs are stable across runs.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		match, err := path.Match(k, name)
		if err == nil && match {
			return m[k], true
		}
	}
	return 0, false
}
