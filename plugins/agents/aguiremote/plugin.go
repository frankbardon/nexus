// Package aguiremote hosts the nexus.agent.agui_remote plugin — the consume-side
// bridge that surfaces one or more configured REMOTE AG-UI agents inside Nexus as
// delegate/subagent targets. Each configured remote agent is registered as an
// LLM-facing tool (default name "delegate_agui_<name>"); when the parent agent
// calls it, the plugin builds an AG-UI RunAgentInput from the delegated task,
// runs the remote agent through the reusable pkg/agui/aguiclient (E4-S1), maps the
// remote run's event stream back onto the Nexus bus (text deltas -> io.output,
// tool activity + iterations -> subagent.* observability), and returns the remote
// run's terminal outcome as the tool.result the parent expects.
//
// It reuses the SAME tool-invoke seam the local agents/delegate and
// agents/subagent plugins use: from the parent agent's perspective a remote
// AG-UI call is a single tool call, just like a local delegate. Budgets/depth are
// honored via a per-call timeout and by riding the bus's causation stack
// (AgentID + Depth) so remote runs slot into the causation tree beneath the
// caller. A per-agent result cache mirrors delegate's content-addressable cache
// so identical tasks replay without re-hitting the remote endpoint.
//
// All comms are event-bus only. The remote transport is the AG-UI wire (HTTP POST
// + SSE) via aguiclient; there is no direct plugin-to-plugin call.
package aguiremote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/agui/aguiclient"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID = "nexus.agent.agui_remote"
	name     = "Remote AG-UI Agents"
	version  = "0.1.0"

	defaultTimeout   = 120 * time.Second
	defaultCacheSize = 128
)

// remoteAgent is a single configured remote AG-UI endpoint exposed as a tool.
type remoteAgent struct {
	name        string
	toolName    string
	description string
	endpoint    string
	bearer      string
	timeout     time.Duration
}

// Plugin registers a delegate-style tool per configured remote AG-UI agent.
type Plugin struct {
	logger *slog.Logger
	bus    engine.EventBus

	agents map[string]*remoteAgent // toolName -> agent
	client func(*remoteAgent) *aguiclient.Client

	cache         *resultCache
	cacheDisabled bool

	unsubs []func()
}

// New returns a default-configured Plugin.
func New() engine.Plugin {
	return &Plugin{
		agents: map[string]*remoteAgent{},
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return name }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus

	cacheSize := defaultCacheSize
	if v, ok := asInt(ctx.Config["cache_size"]); ok && v >= 0 {
		cacheSize = v
	}
	if v, ok := ctx.Config["cache"].(bool); ok {
		p.cacheDisabled = !v
	}
	if !p.cacheDisabled {
		p.cache = newResultCache(cacheSize)
	}

	defTimeout := defaultTimeout
	if v, ok := asInt(ctx.Config["timeout_seconds"]); ok && v > 0 {
		defTimeout = time.Duration(v) * time.Second
	}

	rawAgents, ok := ctx.Config["agents"].([]any)
	if !ok || len(rawAgents) == 0 {
		return fmt.Errorf("agui_remote: config requires a non-empty 'agents' list")
	}
	for i, raw := range rawAgents {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agui_remote: agents[%d] must be a mapping", i)
		}
		ra, err := parseAgent(m, defTimeout)
		if err != nil {
			return fmt.Errorf("agui_remote: agents[%d]: %w", i, err)
		}
		if _, dup := p.agents[ra.toolName]; dup {
			return fmt.Errorf("agui_remote: duplicate tool name %q (from agent %q)", ra.toolName, ra.name)
		}
		p.agents[ra.toolName] = ra
	}

	// Default client factory: one aguiclient bound to the agent's endpoint,
	// bearer token, and a per-agent HTTP timeout. Overridable in tests.
	if p.client == nil {
		p.client = func(ra *remoteAgent) *aguiclient.Client {
			// The stream context carries the per-call deadline; give the HTTP
			// client a generous ceiling above it so a slow SSE stream isn't cut
			// off by the transport before the run-level timeout fires.
			hc := &http.Client{Timeout: ra.timeout + 30*time.Second}
			opts := []aguiclient.Option{aguiclient.WithHTTPClient(hc)}
			if ra.bearer != "" {
				opts = append(opts, aguiclient.WithBearer(ra.bearer))
			}
			return aguiclient.New(ra.endpoint, opts...)
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.onToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	for _, ra := range p.agents {
		desc := ra.description
		if desc == "" {
			desc = fmt.Sprintf("Delegate a subtask to the remote AG-UI agent %q. Runs the remote agent to completion and returns its final response.", ra.name)
		}
		if err := p.bus.Emit("tool.register", events.ToolDef{
			Name:        ra.toolName,
			Description: desc,
			Class:       "agents",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "Natural-language description of what the remote agent should accomplish.",
					},
					"context": map[string]any{
						"type":                 "object",
						"description":          "Structured context passed to the remote agent alongside the task.",
						"additionalProperties": true,
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Override the remote agent's default timeout (seconds) for this call.",
					},
				},
				"required": []string{"task"},
			},
		}); err != nil {
			return fmt.Errorf("agui_remote: register tool %q: %w", ra.toolName, err)
		}
	}
	return nil
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
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"tool.result",
		"before:tool.result",
		"io.output",
		"subagent.started",
		"subagent.iteration",
		"subagent.complete",
	}
}

// onToolInvoke fires for every tool.invoke; it only acts on calls whose Name
// matches one of the configured remote agents' tool names.
func (p *Plugin) onToolInvoke(ev engine.Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok {
		return
	}
	ra, ok := p.agents[tc.Name]
	if !ok {
		return
	}

	task, _ := tc.Arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		p.respondError(tc, "task is required")
		return
	}
	var contextMap map[string]any
	if raw, ok := tc.Arguments["context"].(map[string]any); ok {
		contextMap = raw
	}
	timeout := ra.timeout
	if v, ok := asInt(tc.Arguments["timeout_seconds"]); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}

	// Snapshot the caller's causation identity/depth so the remote run's
	// mapped bus events slot beneath the caller in the causation tree, the
	// same way delegate/subagent do for local sub-runs.
	depth := 0
	if cc, ok := p.bus.(engine.CausationController); ok {
		depth = cc.CurrentCausationContext().Depth
	}

	// Run in a goroutine so the synchronous bus dispatch loop is not blocked
	// across what may be a multi-second remote run.
	go func() {
		out := p.runRemote(ra, task, contextMap, timeout, tc.TurnID, depth)
		result := events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            tc.ID,
			Name:          tc.Name,
			Output:        out.result,
			TurnID:        tc.TurnID,
		}
		if out.err != "" {
			result.Error = out.err
		}
		if veto, vErr := p.bus.EmitVetoable("before:tool.result", &result); vErr == nil && veto.Vetoed {
			p.logger.Info("agui_remote tool.result vetoed", "reason", veto.Reason)
			return
		}
		_ = p.bus.Emit("tool.result", result)
	}()
}

// remoteOutcome is the terminal result of a remote AG-UI run.
type remoteOutcome struct {
	result string
	err    string
}

// runRemote builds a RunAgentInput, streams the remote agent through the E4-S1
// client, maps its event stream onto the bus, and returns the terminal outcome.
// It pushes a causation context so all mapped events carry the remote sub-run's
// identity and depth. Cache hits short-circuit the whole remote call.
func (p *Plugin) runRemote(ra *remoteAgent, task string, contextMap map[string]any, timeout time.Duration, parentTurnID string, parentDepth int) remoteOutcome {
	spawnID := engine.GenerateID()[:16]
	agentID := "agui_remote/" + ra.name + "/" + spawnID
	depth := parentDepth + 1

	key := p.cacheKey(ra, task, contextMap)
	if p.cache != nil {
		if cached, ok := p.cache.get(key); ok {
			p.logger.Info("agui_remote cache hit", "agent", ra.name, "spawn_id", spawnID)
			// Still surface a started/complete pair so observers see the call.
			p.emitStarted(spawnID, task, parentTurnID)
			p.emitComplete(spawnID, parentTurnID, cached.result, cached.err)
			return cached
		}
	}

	if cc, ok := p.bus.(engine.CausationController); ok {
		pop := cc.PushCausationContext(engine.CausationContext{AgentID: agentID, Depth: depth})
		defer pop()
	}

	p.emitStarted(spawnID, task, parentTurnID)

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	input := buildRunInput(spawnID, task, contextMap)
	client := p.client(ra)

	stream, err := client.Stream(ctx, input)
	if err != nil {
		out := remoteOutcome{err: fmt.Sprintf("remote agui transport error: %v", err)}
		p.emitComplete(spawnID, parentTurnID, "", out.err)
		return out
	}
	defer stream.Close()

	out := p.pumpStream(ra, spawnID, parentTurnID, stream)

	// A non-2xx rejection (auth/CORS) carries no events; surface it cleanly.
	if code := stream.Result().StatusCode; code != 0 && (code < 200 || code >= 300) && out.err == "" && out.result == "" {
		out.err = fmt.Sprintf("remote agui rejected request: HTTP %d", code)
	}

	if p.cache != nil && out.err == "" {
		p.cache.put(key, out)
	}
	p.emitComplete(spawnID, parentTurnID, out.result, out.err)
	return out
}

// pumpStream ranges the remote event stream, maps each AG-UI event onto the
// Nexus bus, and derives the terminal outcome from the accumulated text and the
// stream's terminal Result/Err. Text deltas become io.output; tool activity and
// per-message boundaries become subagent.* observability events.
func (p *Plugin) pumpStream(ra *remoteAgent, spawnID, parentTurnID string, stream *aguiclient.Stream) remoteOutcome {
	turnID := "agui_remote_" + spawnID
	var text strings.Builder
	iteration := 0

	// Tool-call accumulation: AG-UI streams tool args incrementally.
	type pendingCall struct {
		name string
		args strings.Builder
	}
	pending := map[string]*pendingCall{}

	for ev := range stream.Events() {
		switch e := ev.(type) {
		case *agui.TextMessageContentEvent:
			if e.Delta != "" {
				text.WriteString(e.Delta)
				p.emitOutput(e.Delta, turnID)
			}
		case *agui.TextMessageChunkEvent:
			if e.Delta != "" {
				text.WriteString(e.Delta)
				p.emitOutput(e.Delta, turnID)
			}
		case *agui.TextMessageEndEvent:
			// Message boundary -> an observability iteration for this worker.
			p.emitIteration(spawnID, parentTurnID, iteration, text.String(), nil)
			iteration++
		case *agui.ToolCallStartEvent:
			pending[e.ToolCallID] = &pendingCall{name: e.ToolCallName}
		case *agui.ToolCallArgsEvent:
			if pc := pending[e.ToolCallID]; pc != nil {
				pc.args.WriteString(e.Delta)
			}
		case *agui.ToolCallChunkEvent:
			pc := pending[e.ToolCallID]
			if pc == nil {
				pc = &pendingCall{name: e.ToolCallName}
				pending[e.ToolCallID] = pc
			}
			pc.args.WriteString(e.Delta)
		case *agui.ToolCallEndEvent:
			if pc := pending[e.ToolCallID]; pc != nil {
				p.emitIteration(spawnID, parentTurnID, iteration, "", []events.ToolCallRequest{{
					ID:        e.ToolCallID,
					Name:      pc.name,
					Arguments: pc.args.String(),
				}})
				iteration++
				delete(pending, e.ToolCallID)
			}
		case *agui.RunErrorEvent:
			msg := e.Message
			if e.Code != "" {
				msg = e.Code + ": " + msg
			}
			return remoteOutcome{result: text.String(), err: "remote agui run error: " + msg}
		}
	}

	// Stream closed: inspect terminal error/outcome.
	if err := stream.Err(); err != nil {
		return remoteOutcome{result: text.String(), err: fmt.Sprintf("remote agui stream error: %v", err)}
	}

	res := stream.Result()
	if in, ok := res.Interrupt(); ok {
		// The parent Nexus agent cannot resolve a remote interrupt in this
		// one-shot delegate flow; report it as a clean, actionable error.
		return remoteOutcome{
			result: text.String(),
			err:    "remote agui agent interrupted awaiting input: " + in.Prompt,
		}
	}
	if res.Outcome() == agui.OutcomeCancelled {
		return remoteOutcome{result: text.String(), err: "remote agui run cancelled"}
	}

	final := strings.TrimSpace(text.String())
	if final == "" {
		final = extractRunResult(res)
	}
	return remoteOutcome{result: final}
}

// --- bus emission helpers ---

func (p *Plugin) emitOutput(content, turnID string) {
	_ = p.bus.Emit("io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       content,
		Role:          "assistant",
		TurnID:        turnID,
	})
}

func (p *Plugin) emitStarted(spawnID, task, parentTurnID string) {
	_ = p.bus.Emit("subagent.started", events.SubagentStarted{
		SchemaVersion: events.SubagentStartedVersion,
		SpawnID:       spawnID,
		Task:          task,
		ParentTurnID:  parentTurnID,
	})
}

func (p *Plugin) emitIteration(spawnID, parentTurnID string, iteration int, content string, calls []events.ToolCallRequest) {
	_ = p.bus.Emit("subagent.iteration", events.SubagentIteration{
		SchemaVersion: events.SubagentIterationVersion,
		SpawnID:       spawnID,
		Iteration:     iteration,
		Content:       content,
		ToolCalls:     calls,
		ParentTurnID:  parentTurnID,
	})
}

func (p *Plugin) emitComplete(spawnID, parentTurnID, result, errMsg string) {
	_ = p.bus.Emit("subagent.complete", events.SubagentComplete{
		SchemaVersion: events.SubagentCompleteVersion,
		SpawnID:       spawnID,
		Result:        result,
		Error:         errMsg,
		ParentTurnID:  parentTurnID,
	})
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

// --- helpers ---

// buildRunInput assembles the AG-UI RunAgentInput from the delegated task,
// wrapping structured context in an XML boundary per the house convention.
func buildRunInput(spawnID, task string, contextMap map[string]any) agui.RunAgentInput {
	content := task
	if len(contextMap) > 0 {
		if ctxJSON, err := json.MarshalIndent(contextMap, "", "  "); err == nil {
			content = "<delegate_context>\n" + string(ctxJSON) + "\n</delegate_context>\n\n<task>\n" + task + "\n</task>"
		}
	}
	return agui.RunAgentInput{
		ThreadID: "agui-remote-" + spawnID,
		RunID:    spawnID,
		Messages: []agui.Message{{ID: "m1", Role: "user", Content: content}},
	}
}

// extractRunResult pulls a human-readable string from a RunFinished result
// payload when the stream carried no text deltas (some agents return only a
// structured result). Falls back to the raw JSON, then empty.
func extractRunResult(res aguiclient.Result) string {
	fin := res.First(agui.EventRunFinished)
	if fin == nil {
		return ""
	}
	fe, ok := fin.(*agui.RunFinishedEvent)
	if !ok || len(fe.Result) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(fe.Result, &s); err == nil {
		return s
	}
	return string(fe.Result)
}

func parseAgent(m map[string]any, defTimeout time.Duration) (*remoteAgent, error) {
	nm, _ := m["name"].(string)
	nm = strings.TrimSpace(nm)
	if nm == "" {
		return nil, fmt.Errorf("'name' is required")
	}
	endpoint, _ := m["endpoint"].(string)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("'endpoint' is required")
	}

	ra := &remoteAgent{
		name:     nm,
		endpoint: endpoint,
		timeout:  defTimeout,
	}
	if td, ok := m["tool_name"].(string); ok && strings.TrimSpace(td) != "" {
		ra.toolName = strings.TrimSpace(td)
	} else {
		ra.toolName = "delegate_agui_" + sanitizeToolSuffix(nm)
	}
	if desc, ok := m["description"].(string); ok {
		ra.description = desc
	}
	if v, ok := asInt(m["timeout_seconds"]); ok && v > 0 {
		ra.timeout = time.Duration(v) * time.Second
	}

	// Auth: explicit token wins, else read from named env var. Secrets never
	// live in code; bearer_token_env is the recommended form.
	if tok, ok := m["bearer_token"].(string); ok && tok != "" {
		ra.bearer = tok
	} else if env, ok := m["bearer_token_env"].(string); ok && env != "" {
		ra.bearer = os.Getenv(env)
	}

	return ra, nil
}

// sanitizeToolSuffix lowercases and replaces non-alphanumeric runs with '_' so a
// human-friendly agent name yields a valid tool identifier.
func sanitizeToolSuffix(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// cacheKey is a content-addressable key over endpoint + task + canonicalized
// context, mirroring delegate's cache key so identical calls replay.
func (p *Plugin) cacheKey(ra *remoteAgent, task string, contextMap map[string]any) string {
	h := sha256.New()
	h.Write([]byte(ra.endpoint))
	h.Write([]byte{0})
	h.Write([]byte(task))
	h.Write([]byte{0})
	if len(contextMap) > 0 {
		keys := make([]string, 0, len(contextMap))
		for k := range contextMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			if data, err := json.Marshal(contextMap[k]); err == nil {
				h.Write(data)
			}
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
