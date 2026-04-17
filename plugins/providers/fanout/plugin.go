// Package fanout sends a single LLM request to multiple providers in parallel
// and collects their responses. The plugin supports configurable selection
// strategies to determine the final response returned to the agent.
//
// The plugin uses three subscription points:
//   - before:llm.request (priority 2): detects fanout roles, vetoes the
//     original request, and dispatches parallel requests via EmitAsync.
//   - llm.response (priority 1): collects individual provider responses
//     tagged with _fanout_id metadata, suppressing them from the agent.
//   - before:core.error (priority 4): captures provider errors within a
//     fanout sequence so failed legs don't surface as session errors.
package fanout

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.provider.fanout"

const (
	StrategyAll       = "all"
	StrategyLLMJudge  = "llm_judge"
	StrategyHeuristic = "heuristic"
	StrategyUser      = "user"
)

// config holds plugin configuration.
type config struct {
	Strategy       string        `yaml:"strategy"`
	DeadlineMillis int           `yaml:"deadline_ms"`
	deadline       time.Duration // parsed from DeadlineMillis

	// Heuristic strategy settings.
	HeuristicPrefer        string `yaml:"heuristic_prefer"`         // "longest" | "shortest" | "fastest" | "cheapest"
	HeuristicRequireFinish bool   `yaml:"heuristic_require_finish"` // discard responses with finish_reason != "end_turn"

	// LLM judge strategy settings.
	JudgeRole string `yaml:"judge_role"` // model role for the judge call
}

// fanoutState tracks an in-flight fanout sequence.
type fanoutState struct {
	mu         sync.Mutex
	role       string
	strategy   string
	targets    []events.ProviderFanoutTarget
	request    events.LLMRequest
	responses  []events.LLMResponse
	receivedAt []time.Time // parallel to responses — when each response arrived
	errors     []fanoutError
	done       chan struct{} // closed when all responses collected or deadline hit
	expected   int
	received   int
}

type fanoutError struct {
	provider string
	model    string
	err      string
}

// Plugin coordinates provider fanout.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	models *engine.ModelRegistry
	cfg    config
	unsubs []func()

	mu             sync.Mutex
	inflight       map[string]*fanoutState // keyed by fanout ID
	pendingChoices map[string]chan int      // user strategy: fanoutID -> chosen index
}

// New creates a new fanout coordinator plugin.
func New() engine.Plugin {
	return &Plugin{
		inflight:       make(map[string]*fanoutState),
		pendingChoices: make(map[string]chan int),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Provider Fanout" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		// Intercept before gates (priority 2, before fallback at 3, gates at 10).
		{EventType: "before:llm.request", Priority: 2},
		// Collect responses before agent handlers (priority 1, agent at 50).
		{EventType: "llm.response", Priority: 1},
		// Capture provider errors within fanout sequences.
		{EventType: "before:core.error", Priority: 4},
		// User strategy: receive user's choice.
		{EventType: "provider.fanout.chosen", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"llm.response",
		"provider.fanout.start",
		"provider.fanout.response",
		"provider.fanout.complete",
		"provider.fanout.choose",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.models = ctx.Models

	// Parse config with defaults.
	p.cfg = config{
		Strategy:       StrategyAll,
		DeadlineMillis: 30000, // 30s default
	}
	if v, ok := ctx.Config["strategy"].(string); ok {
		p.cfg.Strategy = v
	}
	if v, ok := ctx.Config["deadline_ms"].(int); ok {
		p.cfg.DeadlineMillis = v
	} else if v, ok := ctx.Config["deadline_ms"].(float64); ok {
		p.cfg.DeadlineMillis = int(v)
	}
	p.cfg.deadline = time.Duration(p.cfg.DeadlineMillis) * time.Millisecond

	// Parse heuristic sub-config.
	p.cfg.HeuristicPrefer = "longest" // default
	if hm, ok := ctx.Config["heuristic"].(map[string]any); ok {
		if v, ok := hm["prefer"].(string); ok {
			p.cfg.HeuristicPrefer = v
		}
		if v, ok := hm["require_finish"].(bool); ok {
			p.cfg.HeuristicRequireFinish = v
		}
	}

	// Parse judge config.
	if v, ok := ctx.Config["judge_role"].(string); ok {
		p.cfg.JudgeRole = v
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleResponse, engine.WithPriority(1), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
		p.bus.Subscribe("provider.fanout.chosen", p.handleUserChoice, engine.WithSource(pluginID)),
	)

	p.logger.Info("provider fanout plugin initialized",
		"strategy", p.cfg.Strategy,
		"deadline_ms", p.cfg.DeadlineMillis,
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

// handleBeforeRequest detects fanout roles, vetoes the original request,
// and dispatches parallel requests to all providers in the fanout group.
func (p *Plugin) handleBeforeRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip requests already in a fanout sequence.
	if req.Metadata != nil {
		if _, ok := req.Metadata["_fanout_id"]; ok {
			return
		}
	}

	role := req.Role
	if !p.models.IsFanout(role) {
		return
	}

	providers := p.models.FanoutProviders(role)
	if len(providers) == 0 {
		return
	}

	// Veto the original request — we'll re-emit targeted copies.
	fanoutID := engine.GenerateID()
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("fanout: dispatching to %d providers", len(providers)),
	}

	// Build targets list for the event.
	targets := make([]events.ProviderFanoutTarget, len(providers))
	for i, pc := range providers {
		targets[i] = events.ProviderFanoutTarget{
			Provider: pc.Provider,
			Model:    pc.Model,
		}
	}

	// Store state.
	state := &fanoutState{
		role:     role,
		strategy: p.cfg.Strategy,
		targets:  targets,
		request:  *req,
		done:     make(chan struct{}),
		expected: len(providers),
	}

	p.mu.Lock()
	p.inflight[fanoutID] = state
	p.mu.Unlock()

	p.logger.Info("fanout started",
		"fanout_id", fanoutID,
		"role", role,
		"strategy", p.cfg.Strategy,
		"providers", len(providers),
	)

	// Notify observers.
	_ = p.bus.Emit("provider.fanout.start", events.ProviderFanoutStart{
		FanoutID: fanoutID,
		Role:     role,
		Strategy: p.cfg.Strategy,
		Targets:  targets,
	})

	// Dispatch parallel requests.
	for _, pc := range providers {
		newMeta := make(map[string]any)
		if req.Metadata != nil {
			for k, v := range req.Metadata {
				newMeta[k] = v
			}
		}
		newMeta["_fanout_id"] = fanoutID
		newMeta["_target_provider"] = pc.Provider
		newMeta["_fanout_provider"] = pc.Provider
		// Tag with _source so agent handlers skip individual fanout responses.
		// The final combined response emitted by finalize() has no _source.
		newMeta["_source"] = pluginID

		fanReq := *req
		fanReq.Model = pc.Model
		fanReq.Metadata = newMeta
		fanReq.Stream = false // fanout requires complete responses
		if pc.MaxTokens > 0 && fanReq.MaxTokens == 0 {
			fanReq.MaxTokens = pc.MaxTokens
		}

		p.bus.EmitAsync("llm.request", fanReq)
	}

	// Start deadline watcher.
	go p.watchDeadline(fanoutID)
}

// handleResponse intercepts llm.response events tagged with a fanout ID,
// collects them, and emits the final combined response when all are in.
func (p *Plugin) handleResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}

	// Skip judge responses — they go through the one-shot sub in selectByJudge.
	if _, isJudge := resp.Metadata["_fanout_judge"]; isJudge {
		return
	}

	fanoutID, _ := resp.Metadata["_fanout_id"].(string)
	if fanoutID == "" {
		return
	}

	provider, _ := resp.Metadata["_fanout_provider"].(string)

	p.mu.Lock()
	state, exists := p.inflight[fanoutID]
	p.mu.Unlock()

	if !exists {
		return
	}

	state.mu.Lock()
	state.responses = append(state.responses, resp)
	state.receivedAt = append(state.receivedAt, time.Now())
	state.received++
	allDone := state.received >= state.expected
	state.mu.Unlock()

	p.logger.Info("fanout response received",
		"fanout_id", fanoutID,
		"provider", provider,
		"model", resp.Model,
		"received", state.received,
		"expected", state.expected,
	)

	// Notify per-provider response.
	_ = p.bus.Emit("provider.fanout.response", events.ProviderFanoutResponse{
		FanoutID: fanoutID,
		Provider: provider,
		Model:    resp.Model,
		Success:  true,
	})

	if allDone {
		p.finalize(fanoutID, state)
	}
}

// handleBeforeError captures provider errors for in-flight fanout requests.
func (p *Plugin) handleBeforeError(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	errInfo, ok := vp.Original.(*events.ErrorInfo)
	if !ok {
		return
	}

	meta := errInfo.RequestMeta
	if meta == nil {
		return
	}

	fanoutID, _ := meta["_fanout_id"].(string)
	if fanoutID == "" {
		return
	}

	provider, _ := meta["_fanout_provider"].(string)
	model, _ := meta["_target_provider"].(string)

	p.mu.Lock()
	state, exists := p.inflight[fanoutID]
	p.mu.Unlock()

	if !exists {
		return
	}

	// Veto the error — we're handling it within the fanout.
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("fanout: absorbing error from %s", provider),
	}

	state.mu.Lock()
	state.errors = append(state.errors, fanoutError{
		provider: provider,
		model:    model,
		err:      errInfo.Err.Error(),
	})
	state.received++
	allDone := state.received >= state.expected
	state.mu.Unlock()

	p.logger.Warn("fanout provider failed",
		"fanout_id", fanoutID,
		"provider", provider,
		"error", errInfo.Err,
	)

	// Notify per-provider failure.
	_ = p.bus.Emit("provider.fanout.response", events.ProviderFanoutResponse{
		FanoutID: fanoutID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    errInfo.Err.Error(),
	})

	if allDone {
		p.finalize(fanoutID, state)
	}
}

// handleUserChoice receives the user's selection for the "user" strategy.
func (p *Plugin) handleUserChoice(event engine.Event[any]) {
	chosen, ok := event.Payload.(events.ProviderFanoutChosen)
	if !ok {
		return
	}

	p.mu.Lock()
	ch, exists := p.pendingChoices[chosen.FanoutID]
	p.mu.Unlock()

	if !exists {
		return
	}

	select {
	case ch <- chosen.ChosenIndex:
	default:
	}
}

// watchDeadline enforces the configured deadline for a fanout sequence.
func (p *Plugin) watchDeadline(fanoutID string) {
	timer := time.NewTimer(p.cfg.deadline)
	defer timer.Stop()

	p.mu.Lock()
	state, exists := p.inflight[fanoutID]
	p.mu.Unlock()
	if !exists {
		return
	}

	select {
	case <-timer.C:
		state.mu.Lock()
		remaining := state.expected - state.received
		state.mu.Unlock()

		if remaining > 0 {
			p.logger.Warn("fanout deadline reached",
				"fanout_id", fanoutID,
				"remaining", remaining,
			)
			p.finalize(fanoutID, state)
		}
	case <-state.done:
		// Already finalized.
	}
}

// finalize assembles the combined response and emits it.
func (p *Plugin) finalize(fanoutID string, state *fanoutState) {
	// Ensure single finalization.
	select {
	case <-state.done:
		return // already finalized
	default:
		close(state.done)
	}

	// Remove from inflight.
	p.mu.Lock()
	delete(p.inflight, fanoutID)
	p.mu.Unlock()

	state.mu.Lock()
	responses := make([]events.LLMResponse, len(state.responses))
	copy(responses, state.responses)
	receivedAt := make([]time.Time, len(state.receivedAt))
	copy(receivedAt, state.receivedAt)
	errors := make([]fanoutError, len(state.errors))
	copy(errors, state.errors)
	origRequest := state.request
	state.mu.Unlock()

	if len(responses) == 0 {
		// All providers failed — emit error.
		errMsg := "fanout: all providers failed"
		if len(errors) > 0 {
			errMsg = fmt.Sprintf("fanout: all %d providers failed, first: %s", len(errors), errors[0].err)
		}
		_ = p.bus.Emit("core.error", &events.ErrorInfo{
			Source:      pluginID,
			Err:         fmt.Errorf("%s", errMsg),
			Fatal:       false,
			RequestMeta: origRequest.Metadata,
		})
		return
	}

	// Apply selection strategy.
	switch state.strategy {
	case StrategyHeuristic:
		if len(responses) > 1 {
			winner := p.selectByHeuristic(responses, receivedAt)
			if winner != 0 {
				responses[0], responses[winner] = responses[winner], responses[0]
				receivedAt[0], receivedAt[winner] = receivedAt[winner], receivedAt[0]
			}
		}
		p.emitFinalResponse(fanoutID, state, responses)

	case StrategyLLMJudge:
		if len(responses) > 1 {
			// Async — judge makes an LLM call then emits the final response.
			go p.selectByJudge(fanoutID, state, responses, origRequest)
			return
		}
		p.emitFinalResponse(fanoutID, state, responses)

	case StrategyUser:
		if len(responses) > 1 {
			// Async — waits for user to pick, then emits the final response.
			p.presentToUser(fanoutID, state, responses)
			return
		}
		p.emitFinalResponse(fanoutID, state, responses)

	default: // StrategyAll
		p.emitFinalResponse(fanoutID, state, responses)
	}
}

// emitFinalResponse builds the combined response and emits it. First response
// in the slice is primary, rest become alternatives.
func (p *Plugin) emitFinalResponse(fanoutID string, state *fanoutState, responses []events.LLMResponse) {
	primary := responses[0]

	// Strip fanout metadata from primary.
	cleanMeta := make(map[string]any)
	for k, v := range primary.Metadata {
		if k == "_fanout_id" || k == "_fanout_provider" || k == "_target_provider" {
			continue
		}
		cleanMeta[k] = v
	}
	cleanMeta["_fanout"] = true
	cleanMeta["_fanout_id"] = fanoutID
	primary.Metadata = cleanMeta

	// Remaining responses become alternatives.
	if len(responses) > 1 {
		primary.Alternatives = responses[1:]
	}

	// Aggregate usage across all responses.
	var totalUsage events.Usage
	var totalCost float64
	for _, r := range responses {
		totalUsage.PromptTokens += r.Usage.PromptTokens
		totalUsage.CompletionTokens += r.Usage.CompletionTokens
		totalUsage.TotalTokens += r.Usage.TotalTokens
		totalCost += r.CostUSD
	}
	primary.Usage = totalUsage
	primary.CostUSD = totalCost

	p.logger.Info("fanout complete",
		"fanout_id", fanoutID,
		"succeeded", len(responses),
		"strategy", state.strategy,
	)

	// Notify completion.
	_ = p.bus.Emit("provider.fanout.complete", events.ProviderFanoutComplete{
		FanoutID:  fanoutID,
		Role:      state.role,
		Strategy:  state.strategy,
		Succeeded: len(responses),
	})

	// Emit combined response. This goes to the agent as a normal llm.response.
	_ = p.bus.Emit("llm.response", primary)
}
