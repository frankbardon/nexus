// Package testio provides a non-interactive IO plugin for integration testing.
// It replaces nexus.io.tui in test configs, feeding scripted inputs, collecting
// all bus events, and handling approvals per scenario configuration.
//
// When mock_responses is configured, the plugin intercepts LLM requests and
// injects synthetic responses — no real LLM calls, no API key needed.
//
// The plugin is designed to be driven by pkg/testharness but can also be used
// standalone in any test config.
package testio

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.io.test"

// ApprovalRule controls how the plugin responds to approval requests.
type ApprovalRule struct {
	Match  string // substring match on tool call or description
	Action string // "approve" or "deny"
}

// MockResponse is a synthetic LLM response injected instead of a real API call.
type MockResponse struct {
	Content   string
	ToolCalls []events.ToolCallRequest
}

// hitlScript is a scripted response to a single hitl.requested event.
// Bare-string config entries are normalized into FreeText; map entries
// can populate either ChoiceID or FreeText.
type hitlScript struct {
	ChoiceID string
	FreeText string
}

// Plugin is the test IO plugin. It feeds scripted inputs into the engine,
// collects all bus events, and auto-responds to approvals per config.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	unsubs  []func()

	// Config
	inputs        []string
	inputDelay    time.Duration
	approvalMode  string // "approve", "deny", "per-prompt"
	approvalRules []ApprovalRule
	hitlResponses []hitlScript
	mockResponses []MockResponse
	timeout       time.Duration

	// State
	mu            sync.Mutex
	collected     []CollectedEvent
	inputsSent    int
	turnDepth     int
	turnsSeen     int
	mockIndex     int
	allInputsSent bool
	finalized     bool
	done          chan struct{} // closed when session ends
}

// CollectedEvent is a bus event captured during the test run.
type CollectedEvent struct {
	Type      string
	Source    string
	Timestamp time.Time
	Payload   any
}

// New creates a new test IO plugin instance.
func New() engine.Plugin {
	return &Plugin{
		done: make(chan struct{}),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Test IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	subs := []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "io.approval.request", Priority: 10},
		{EventType: "plan.approval.request", Priority: 10},
		{EventType: "io.ask", Priority: 10},
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
	}
	if len(p.mockResponses) > 0 {
		subs = append(subs, engine.EventSubscription{
			EventType: "before:llm.request", Priority: 20,
		})
	}
	return subs
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"plan.approval.response",
		"io.ask.response",
		"io.session.start",
		"io.session.end",
		"llm.response",
	}
}

// Init reads config and wires event handlers.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Parse config.
	if v, ok := ctx.Config["inputs"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				p.inputs = append(p.inputs, s)
			}
		}
	}

	p.inputDelay = 500 * time.Millisecond
	if v, ok := ctx.Config["input_delay"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			p.inputDelay = d
		}
	}

	p.approvalMode = "approve"
	if v, ok := ctx.Config["approval_mode"].(string); ok {
		p.approvalMode = v
	}

	if v, ok := ctx.Config["approval_rules"].([]any); ok {
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				rule := ApprovalRule{}
				if s, ok := m["match"].(string); ok {
					rule.Match = s
				}
				if s, ok := m["action"].(string); ok {
					rule.Action = s
				}
				p.approvalRules = append(p.approvalRules, rule)
			}
		}
	}

	if v, ok := ctx.Config["hitl_responses"].([]any); ok {
		for _, item := range v {
			switch entry := item.(type) {
			case string:
				p.hitlResponses = append(p.hitlResponses, hitlScript{FreeText: entry})
			case map[string]any:
				script := hitlScript{}
				if s, ok := entry["choice_id"].(string); ok {
					script.ChoiceID = s
				}
				if s, ok := entry["free_text"].(string); ok {
					script.FreeText = s
				}
				p.hitlResponses = append(p.hitlResponses, script)
			}
		}
	}

	p.parseMockResponses(ctx.Config)

	p.timeout = 60 * time.Second
	if v, ok := ctx.Config["timeout"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			p.timeout = d
		}
	}

	// Subscribe to specific events for response handling.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.request", p.handlePlanApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITL, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithSource(pluginID)),
	)

	// Mock LLM responses: intercept before:llm.request after gates (priority 20),
	// and also intercept llm.request directly to catch retry requests from gates
	// (e.g. json_schema retrier emits llm.request, not before:llm.request).
	if len(p.mockResponses) > 0 {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe("before:llm.request", p.handleMockBeforeLLMRequest,
				engine.WithPriority(20), engine.WithSource(pluginID)),
			p.bus.Subscribe("llm.request", p.handleMockLLMRequest,
				engine.WithPriority(5), engine.WithSource(pluginID)),
		)
		p.logger.Info("test IO mock mode enabled", "mock_responses", len(p.mockResponses))
	}

	// Collect ALL events via wildcard subscription.
	p.unsubs = append(p.unsubs,
		p.bus.SubscribeAll(p.collectEvent),
	)

	p.logger.Info("test IO plugin initialized",
		"inputs", len(p.inputs),
		"approval_mode", p.approvalMode,
		"mock", len(p.mockResponses) > 0)
	return nil
}

// Ready emits session start and begins feeding inputs.
func (p *Plugin) Ready() error {
	_ = p.bus.Emit("io.session.start", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, Transport: "test"})

	// Feed inputs asynchronously so Ready returns promptly.
	go p.feedInputs()

	// Timeout watchdog.
	go func() {
		timer := time.NewTimer(p.timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			p.logger.Warn("test IO timeout reached, ending session", "timeout", p.timeout)
			p.endSession()
		case <-p.done:
		}
	}()

	return nil
}

// Shutdown unsubscribes all handlers.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// Done returns a channel that closes when the test session ends.
func (p *Plugin) Done() <-chan struct{} {
	return p.done
}

// Collected returns all events captured during the run. Safe to call after
// Done() is closed.
func (p *Plugin) Collected() []CollectedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]CollectedEvent, len(p.collected))
	copy(out, p.collected)
	return out
}

// -- internal ----------------------------------------------------------------

func (p *Plugin) feedInputs() {
	for i, input := range p.inputs {
		if i > 0 {
			time.Sleep(p.inputDelay)
		}

		// Wait for previous turn to finish before sending next input.
		p.waitForIdle()

		p.mu.Lock()
		p.inputsSent++
		p.mu.Unlock()

		p.logger.Info("test IO sending input", "index", i, "content_len", len(input))
		payload := events.UserInput{SchemaVersion: events.UserInputVersion, Content: input}
		if veto, err := p.bus.EmitVetoable("before:io.input", &payload); err == nil && veto.Vetoed {
			continue
		}
		_ = p.bus.Emit("io.input", payload)
	}

	p.mu.Lock()
	p.allInputsSent = true
	// Check if turns already finished while we were still inside the last
	// synchronous Emit. The bus dispatches handlers inline, so agent.turn.end
	// may have fired before we got here.
	shouldEnd := p.turnDepth == 0 && p.turnsSeen > 0 && !p.finalized
	p.mu.Unlock()

	if shouldEnd {
		go func() {
			time.Sleep(100 * time.Millisecond)
			p.endSession()
		}()
		return
	}

	// Stall detection: if turn depth is stuck (e.g. gate vetoed an LLM request
	// permanently and the agent never emitted agent.turn.end), wait a settle
	// period and end the session. Without this, permanently-vetoed turns hang
	// until the global timeout.
	go func() {
		time.Sleep(3 * time.Second)
		p.mu.Lock()
		stuck := p.allInputsSent && p.turnDepth > 0 && !p.finalized
		p.mu.Unlock()
		if stuck {
			p.logger.Info("test IO detected stalled turn, ending session")
			p.endSession()
		}
	}()
}

func (p *Plugin) waitForIdle() {
	for {
		p.mu.Lock()
		idle := p.turnDepth == 0
		p.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *Plugin) collectEvent(e engine.Event[any]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.collected = append(p.collected, CollectedEvent{
		Type:      e.Type,
		Source:    e.Source,
		Timestamp: e.Timestamp,
		Payload:   e.Payload,
	})
}

// -- mock LLM responses ------------------------------------------------------

// handleMockBeforeLLMRequest intercepts the agent's vetoable LLM request.
// Fires after gates (priority 20 vs gates at 10). Vetoes the request and
// injects a synthetic llm.response so the provider is never called.
func (p *Plugin) handleMockBeforeLLMRequest(e engine.Event[any]) {
	vp, ok := e.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}

	// If already vetoed by a gate (stop words, etc.), don't override.
	if vp.Veto.Vetoed {
		return
	}

	mock := p.nextMockResponse()

	// Veto the real LLM request so it never reaches the provider.
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: "test IO mock: intercepted by mock response",
	}

	// Emit synthetic llm.response in a goroutine — we're inside a vetoable
	// handler, and emitting synchronously could cause ordering issues.
	go func() {
		_ = p.bus.Emit("llm.response", p.buildMockResponse(mock, nil))
	}()
}

// handleMockLLMRequest intercepts direct llm.request emissions (e.g. gate
// retrier sends llm.request directly, bypassing before:llm.request).
// Responds with the next mock and suppresses the real provider call by
// emitting llm.response before the provider handler runs.
func (p *Plugin) handleMockLLMRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.LLMRequest)
	if !ok {
		return
	}

	mock := p.nextMockResponse()

	// Propagate full request metadata so downstream plugins (fanout, retrier)
	// can correlate the response. Matches real provider behavior.
	_ = p.bus.Emit("llm.response", p.buildMockResponse(mock, req.Metadata))
}

func (p *Plugin) nextMockResponse() MockResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.mockResponses) == 0 {
		return MockResponse{}
	}
	mock := p.mockResponses[p.mockIndex]
	if p.mockIndex < len(p.mockResponses)-1 {
		p.mockIndex++
	}
	return mock
}

func (p *Plugin) buildMockResponse(mock MockResponse, metadata map[string]any) events.LLMResponse {
	return events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: mock.Content,
		ToolCalls:    mock.ToolCalls,
		Model:        "mock",
		FinishReason: "end_turn",
		Metadata:     metadata,
	}
}

func (p *Plugin) parseMockResponses(cfg map[string]any) {
	v, ok := cfg["mock_responses"].([]any)
	if !ok {
		return
	}
	for _, item := range v {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		mock := MockResponse{}
		if s, ok := m["content"].(string); ok {
			mock.Content = s
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for i, tc := range tcs {
				tcMap, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				call := events.ToolCallRequest{
					ID: fmt.Sprintf("mock_tc_%d", i),
				}
				if s, ok := tcMap["name"].(string); ok {
					call.Name = s
				}
				if s, ok := tcMap["arguments"].(string); ok {
					call.Arguments = s
				}
				mock.ToolCalls = append(mock.ToolCalls, call)
			}
		}
		p.mockResponses = append(p.mockResponses, mock)
	}
}

// -- approval/ask handlers ---------------------------------------------------

func (p *Plugin) handleApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}

	approved := p.shouldApprove(req.ToolCall, req.Description)

	_ = p.bus.Emit("io.approval.response", events.ApprovalResponse{SchemaVersion: events.ApprovalResponseVersion, PromptID: req.PromptID,
		Approved: approved,
	})
}

func (p *Plugin) handlePlanApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}

	approved := p.shouldApprove("", req.Description)

	_ = p.bus.Emit("plan.approval.response", events.ApprovalResponse{SchemaVersion: events.ApprovalResponseVersion, PromptID: req.PromptID,
		Approved: approved,
	})
}

func (p *Plugin) handleHITL(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}

	p.mu.Lock()
	resp := events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: req.ID}
	if len(p.hitlResponses) > 0 {
		script := p.hitlResponses[0]
		if len(p.hitlResponses) > 1 {
			p.hitlResponses = p.hitlResponses[1:]
		}
		resp.ChoiceID = script.ChoiceID
		resp.FreeText = script.FreeText
	} else if req.DefaultChoiceID != "" {
		resp.ChoiceID = req.DefaultChoiceID
	}
	p.mu.Unlock()

	_ = p.bus.Emit("hitl.responded", resp)
}

// -- turn tracking -----------------------------------------------------------

func (p *Plugin) handleTurnStart(e engine.Event[any]) {
	p.mu.Lock()
	p.turnDepth++
	p.turnsSeen++
	p.mu.Unlock()
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	p.mu.Lock()
	if p.turnDepth > 0 {
		p.turnDepth--
	}
	shouldEnd := p.turnDepth == 0 && p.allInputsSent && p.turnsSeen > 0 && !p.finalized
	p.mu.Unlock()

	if shouldEnd {
		go func() {
			time.Sleep(100 * time.Millisecond)
			p.endSession()
		}()
	}
}

// -- shared helpers ----------------------------------------------------------

func (p *Plugin) shouldApprove(toolCall, description string) bool {
	switch p.approvalMode {
	case "deny":
		return false
	case "per-prompt":
		combined := toolCall + " " + description
		for _, rule := range p.approvalRules {
			if strings.Contains(strings.ToLower(combined), strings.ToLower(rule.Match)) {
				return rule.Action == "approve"
			}
		}
		return true
	default: // "approve"
		return true
	}
}

func (p *Plugin) endSession() {
	p.mu.Lock()
	if p.finalized {
		p.mu.Unlock()
		return
	}
	p.finalized = true
	p.mu.Unlock()

	_ = p.bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, Transport: "test"})
	close(p.done)
}
