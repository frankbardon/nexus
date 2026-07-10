// Package agui implements the nexus.io.agui serve plugin: an HTTP listener that
// exposes the AG-UI ("Agent-User Interaction") endpoint. Clients POST a
// RunAgentInput and receive a text/event-stream SSE response, one stream per
// run. The wire format is defined by pkg/agui (not the pkg/ui Envelope used by
// the browser/wails transports).
//
// # Round-trip
//
// Inbound: a POST RunAgentInput maps its messages to a Nexus io.input on the
// bus (threadId selects/records the session, runId identifies the turn).
// Outbound: the plugin subscribes to the same bus events as the browser
// transport and translates them to canonical AG-UI SSE events for the single
// in-flight run, terminating the stream at RunFinished.
//
// Concurrency model: bus handlers run on arbitrary engine goroutines and never
// touch the SSE writer. Each handler translates its payload and pushes AG-UI
// events onto the active run's buffered channel; the HTTP handler goroutine is
// the sole reader of that channel and the sole writer to the SSE stream. A
// mutex guards the "active run" pointer. This keeps the SSEWriter race-free.
//
// Non-canonical Nexus bus events (workflow.progress, subagent.*,
// code.exec.stdout, ...) have no canonical AG-UI equivalent; they consistently
// ride the AG-UI Custom event (name = bus event type). See
// docs/src/plugins/io-agui.md.
package agui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.io.agui"

// defaultBindAddr binds loopback by default so the endpoint is not exposed on
// the network without explicit operator opt-in.
const defaultBindAddr = "127.0.0.1:8090"

// customBridgedEvents are Nexus-specific bus events with no canonical AG-UI
// equivalent. They ride the AG-UI Custom event (name = bus event type) so a
// conformance client sees a documented superset rather than silently dropping
// them. This is the single, consistent approach chosen for non-canonical events.
var customBridgedEvents = []string{
	"workflow.progress",
	"subagent.started",
	"subagent.iteration",
	"subagent.complete",
	"code.exec.stdout",
}

// Plugin is the AG-UI serve plugin. It stands up an embedded *http.Server that
// exposes the AG-UI POST endpoint with safe-by-default exposure: loopback bind,
// optional bearer-token auth, and configurable CORS. It also owns the bus
// subscriptions that feed the outbound SSE translation.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	server *Server

	sessionID string

	bindAddr    string
	bearerToken string
	corsOrigins []string

	// mu guards active: at most one run is in flight per listener for this
	// scope (single engine/session per listener, mirroring io/browser).
	mu     sync.Mutex
	active *run

	// pendingMu guards pending. A virtual-run interrupt records the mapping
	// from the AG-UI interruptId to the underlying HITL request so the resume
	// side (E2-S2) can correlate a ResumeItem back to the still-blocked
	// in-process hitl and emit the matching hitl.responded. Populated on
	// hitl.requested; the resume handler (E2-S2) is the sole consumer/remover.
	pendingMu sync.Mutex
	pending   map[string]pendingInterrupt

	unsubs []func()
}

// pendingInterrupt records the correlation between an AG-UI interrupt (surfaced
// to the client via a RunFinished(interrupt) outcome) and the in-process HITL
// request it suspended. It is the seam E2-S2 uses on resume: given a
// ResumeItem.InterruptID it looks up the RequestID to emit hitl.responded, and
// the SessionID/TurnID to open the continuation run against the right thread.
type pendingInterrupt struct {
	// InterruptID is the AG-UI-facing id echoed by the client on resume.
	InterruptID string
	// RequestID is the underlying HITLRequest.ID to resolve via hitl.responded.
	RequestID string
	// SessionID / TurnID / ThreadID / RunID scope the interrupt to the thread
	// and turn it suspended so the continuation run targets the same session.
	SessionID string
	TurnID    string
	ThreadID  string
	RunID     string
	// Mode is the HITL response shape, carried so the resume side can validate
	// a ResumeItem payload against what the request accepts.
	Mode events.HITLMode
}

// New creates a new AG-UI serve plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "AG-UI IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares every bus event the outbound translator consumes. It
// must stay in lockstep with the Subscribe calls in Init (the contract harness
// enforces this).
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	subs := []engine.EventSubscription{
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "io.output", Priority: 50},
		{EventType: "tool.call", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		// HITL suspends the run at the transport boundary (virtual-run model):
		// hitl.requested ends the SSE with an interrupt outcome; hitl.cancel
		// ends it with a cancelled outcome. Neither unblocks the in-process
		// agent — that is the resume side's job (E2-S2).
		{EventType: "hitl.requested", Priority: 50},
		{EventType: "hitl.cancel", Priority: 50},
	}
	for _, et := range customBridgedEvents {
		subs = append(subs, engine.EventSubscription{EventType: et, Priority: 50})
	}
	return subs
}

// Emissions declares the event types the inbound handler emits onto the bus.
// hitl.responded is emitted by the resume path (E2-S2): a continuation
// RunAgentInput carrying resume[] resolves the pending interrupt(s) that ended a
// prior run, unblocking the still-parked in-process agent. Both the resolved and
// the cancelled resume statuses ride hitl.responded (Cancelled:true for the
// latter) — the shape the control/hitl waiter matches on.
func (p *Plugin) Emissions() []string {
	return []string{
		"before:io.input",
		"io.input",
		"hitl.responded",
	}
}

// Init reads config, constructs the server, and wires the outbound bus
// subscriptions. Nothing binds a socket here; the listener starts in Ready so
// all plugins have finished Init first.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.pending = make(map[string]pendingInterrupt)

	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
	}

	p.bindAddr = defaultBindAddr
	if v, ok := ctx.Config["bind"].(string); ok && strings.TrimSpace(v) != "" {
		p.bindAddr = strings.TrimSpace(v)
	}

	// Bearer token: an inline `bearer_token` takes precedence; otherwise
	// `bearer_token_env` names an environment variable to read it from. Auth
	// is enforced only when a non-empty token is resolved.
	if v, ok := ctx.Config["bearer_token"].(string); ok {
		p.bearerToken = strings.TrimSpace(v)
	}
	if p.bearerToken == "" {
		if envVar, ok := ctx.Config["bearer_token_env"].(string); ok && strings.TrimSpace(envVar) != "" {
			p.bearerToken = strings.TrimSpace(os.Getenv(strings.TrimSpace(envVar)))
		}
	}

	p.corsOrigins = parseCORSOrigins(ctx.Config["cors_origins"])

	p.server = NewServer(serverConfig{
		addr:        p.bindAddr,
		bearerToken: p.bearerToken,
		corsOrigins: p.corsOrigins,
		logger:      p.logger,
		bridge:      p,
	})

	// Wire outbound bus subscriptions (engine -> AG-UI SSE). Handlers translate
	// and enqueue onto the active run; they never write the SSE directly.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.call", p.handleToolCall, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinkingStep, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequested, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.cancel", p.handleHITLCancel, engine.WithSource(pluginID)),
	)
	for _, et := range customBridgedEvents {
		eventType := et
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(eventType, func(e engine.Event[any]) {
				if r := p.currentRun(); r != nil {
					r.onCustom(eventType, e.Payload)
				}
			}, engine.WithSource(pluginID)),
		)
	}

	p.logger.Info("agui serve plugin initialized",
		"bind", p.bindAddr,
		"auth", p.bearerToken != "",
		"cors_origins", len(p.corsOrigins),
	)
	return nil
}

// Ready starts the HTTP listener.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting agui server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server and unsubscribes from the bus.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil

	// Fail any in-flight run so its HTTP handler returns promptly.
	if r := p.currentRun(); r != nil {
		r.fail("agui server shutting down")
	}

	if p.server == nil {
		return nil
	}
	// Use a fresh context with a deadline: the incoming context may already be
	// cancelled during engine teardown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down agui server: %w", err)
	}
	return nil
}

// --- bridge: inbound (server -> bus) and run lifecycle ---

// startRun registers a new run as the single active run and emits the inbound
// io.input on the bus. It returns the run and an ok flag; ok is false when
// another run is already in flight (one run per listener for this scope), in
// which case the caller must reject with a RunError.
func (p *Plugin) startRun(input runInput) (*run, bool) {
	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, false
	}
	r := newRun(input.threadID, input.runID)
	p.active = r
	p.mu.Unlock()

	// RunStarted is emitted eagerly so even a run with no agent produces a
	// well-formed lifecycle. The first agent.turn.start will not duplicate it.
	r.markStarted()
	r.queue(newRunStarted(input.threadID, input.runID))

	// Map messages -> io.input and publish. The last user message drives the
	// turn; earlier messages are preloaded so resume/history stays intact.
	ui := p.buildUserInput(input)
	go func() {
		if veto, err := p.bus.EmitVetoable("before:io.input", &ui); err == nil && veto.Vetoed {
			r.fail("io.input vetoed")
			return
		}
		if err := p.bus.Emit("io.input", ui); err != nil {
			r.fail(fmt.Sprintf("emit io.input: %v", err))
		}
	}()
	return r, true
}

// endRun clears the active run pointer if it still points at r.
func (p *Plugin) endRun(r *run) {
	p.mu.Lock()
	if p.active == r {
		p.active = nil
	}
	p.mu.Unlock()
}

// currentRun returns the active run or nil.
func (p *Plugin) currentRun() *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// buildUserInput maps a RunAgentInput's messages to a Nexus UserInput. The
// trailing user message becomes Content; any earlier messages ride as
// PreloadMessages so a resumed thread keeps prior context.
func (p *Plugin) buildUserInput(input runInput) events.UserInput {
	sessionID := input.threadID
	if sessionID == "" {
		sessionID = p.sessionID
	}
	ui := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		SessionID:     sessionID,
	}
	msgs := input.messages
	// Find the trailing user message to use as the live turn Content.
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") {
			last = i
			break
		}
	}
	for i, m := range msgs {
		if i == last {
			ui.Content = m.Content
			continue
		}
		ui.PreloadMessages = append(ui.PreloadMessages, events.Message{
			Role:    normalizeRole(m.Role),
			Content: m.Content,
		})
	}
	if last == -1 && len(msgs) > 0 {
		// No user message at all: fall back to the last message's content.
		ui.Content = msgs[len(msgs)-1].Content
		if len(ui.PreloadMessages) > 0 {
			ui.PreloadMessages = ui.PreloadMessages[:len(ui.PreloadMessages)-1]
		}
	}
	return ui
}

// normalizeRole maps AG-UI message roles onto Nexus roles.
func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "assistant", "system", "tool", "user":
		return strings.ToLower(role)
	default:
		return "user"
	}
}

// --- bus handlers (engine -> run channel). Never touch the SSE writer. ---

func (p *Plugin) handleTurnStart(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	r.onTurnStart(t)
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	r.onTurnEnd(t)
	// A top-level turn end terminates the run and the SSE stream.
	r.finish()
}

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	c, ok := e.Payload.(events.StreamChunk)
	if !ok {
		return
	}
	r.onStreamChunk(c)
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	r.onStreamEnd(end)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	o, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	r.onOutput(o)
}

func (p *Plugin) handleToolCall(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	r.onToolCall(tc)
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	tr, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	r.onToolResult(tr)
}

func (p *Plugin) handleThinkingStep(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	s, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	r.onThinkingStep(s)
}

// parseCORSOrigins normalizes the cors_origins config value into a slice of
// trimmed, non-empty origins. It accepts a YAML list ([]any of strings) or a
// single comma-separated string for convenience.
func parseCORSOrigins(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
