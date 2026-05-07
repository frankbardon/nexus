// Package fallback provides automatic provider failover when the primary
// LLM provider returns a non-retryable error or exhausts its retry budget.
//
// The plugin uses two subscription points:
//   - before:llm.request (priority 3): injects fallback tracking metadata into
//     requests for roles with fallback chains, and stores the original request.
//   - before:core.error (priority 5): intercepts provider errors, vetoes them
//     when a fallback is available, and re-emits llm.request targeting the next
//     provider in the chain.
package fallback

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.provider.fallback"

// Plugin coordinates provider fallback on error.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	models *engine.ModelRegistry
	unsubs []func()

	mu       sync.Mutex
	inflight map[string]*fallbackState // keyed by fallback ID
}

// fallbackState tracks an in-flight fallback sequence.
type fallbackState struct {
	role    string
	attempt int
	request events.LLMRequest
}

// New creates a new fallback coordinator plugin.
func New() engine.Plugin {
	return &Plugin{
		inflight: make(map[string]*fallbackState),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Provider Fallback" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		// Inject tracking metadata before gates (priority 3, before gates at 10).
		{EventType: "before:llm.request", Priority: 3},
		// Intercept errors before engine's error→output handler.
		{EventType: "before:core.error", Priority: 5},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.output.clear",
		"provider.fallback",
		"llm.request",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.models = ctx.Models

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("before:core.error", p.handleBeforeError, engine.WithSource(pluginID)),
	)

	p.logger.Info("provider fallback plugin initialized")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

// handleBeforeRequest injects fallback tracking metadata into LLM requests
// for roles that have fallback chains (len > 1). The metadata flows through
// the provider and back via ErrorInfo.RequestMeta on failure.
func (p *Plugin) handleBeforeRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip requests already in a fallback sequence.
	if req.Metadata != nil {
		if _, ok := req.Metadata["_fallback_id"]; ok {
			return
		}
	}

	// Role may be empty — providers resolve "" to the default role,
	// and so do ChainLen/Fallback. Keep it as-is for tracking.
	role := req.Role
	if p.models.ChainLen(role) <= 1 {
		return
	}

	// Inject tracking metadata into the request.
	fallbackID := engine.GenerateID()
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["_fallback_id"] = fallbackID
	req.Metadata["_fallback_attempt"] = 0
	req.Metadata["_fallback_role"] = role

	// Store original request for re-emission on fallback.
	// Deep copy metadata to avoid mutation issues.
	storedMeta := make(map[string]any, len(req.Metadata))
	for k, v := range req.Metadata {
		storedMeta[k] = v
	}
	storedReq := *req
	storedReq.Metadata = storedMeta

	p.mu.Lock()
	p.inflight[fallbackID] = &fallbackState{
		role:    role,
		attempt: 0,
		request: storedReq,
	}
	p.mu.Unlock()
}

// handleBeforeError intercepts provider errors and triggers fallback when
// the error is non-retryable or retries are exhausted and a fallback
// provider exists in the chain.
func (p *Plugin) handleBeforeError(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	errInfo, ok := vp.Original.(*events.ErrorInfo)
	if !ok {
		return
	}

	// Only intercept provider errors (nexus.llm.* sources).
	if !strings.HasPrefix(errInfo.Source, "nexus.llm.") {
		return
	}

	// Only intercept when error is non-retryable, or retries are exhausted.
	if errInfo.Retryable && !errInfo.RetriesExhausted {
		return
	}

	// Extract fallback tracking metadata from the original request.
	meta := errInfo.RequestMeta
	if meta == nil {
		return
	}

	fallbackID, _ := meta["_fallback_id"].(string)
	if fallbackID == "" {
		return
	}

	role, _ := meta["_fallback_role"].(string)
	attempt, _ := meta["_fallback_attempt"].(int)

	// Recover state from inflight tracking.
	p.mu.Lock()
	state, exists := p.inflight[fallbackID]
	if exists {
		if role == "" {
			role = state.role
		}
		if attempt == 0 && state.attempt > 0 {
			attempt = state.attempt
		}
	}
	p.mu.Unlock()

	if !exists {
		return
	}

	// Try next in chain.
	nextAttempt := attempt + 1
	nextCfg, ok := p.models.Fallback(role, nextAttempt)
	if !ok {
		// Chain exhausted. Clean up and let error propagate.
		p.mu.Lock()
		delete(p.inflight, fallbackID)
		p.mu.Unlock()
		return
	}

	// Determine failed provider/model for notification.
	failedProvider := errInfo.Source
	failedModel := ""
	if currentCfg, ok := p.models.Fallback(role, attempt); ok {
		failedModel = currentCfg.Model
	}

	// Veto the error — we're handling it.
	vp.Veto = engine.VetoResult{
		Vetoed: true,
		Reason: fmt.Sprintf("fallback: switching from %s to %s/%s", failedProvider, nextCfg.Provider, nextCfg.Model),
	}

	p.logger.Info("provider fallback triggered",
		"role", role,
		"failed_provider", failedProvider,
		"failed_model", failedModel,
		"next_provider", nextCfg.Provider,
		"next_model", nextCfg.Model,
		"attempt", nextAttempt,
	)

	// Emit io.output.clear to wipe partial streamed content.
	_ = p.bus.Emit("io.output.clear", nil)

	// Emit provider.fallback notification for UI.
	_ = p.bus.Emit("provider.fallback", events.ProviderFallback{SchemaVersion: events.ProviderFallbackVersion, Role: role,
		FailedProvider: failedProvider,
		FailedModel:    failedModel,
		Error:          errInfo.Err.Error(),
		NextProvider:   nextCfg.Provider,
		NextModel:      nextCfg.Model,
		Attempt:        nextAttempt,
	})

	// Update inflight state.
	p.mu.Lock()
	state.attempt = nextAttempt
	origReq := state.request
	p.mu.Unlock()

	// Build new metadata with updated fallback tracking.
	newMeta := make(map[string]any, len(origReq.Metadata))
	for k, v := range origReq.Metadata {
		newMeta[k] = v
	}
	newMeta["_fallback_attempt"] = nextAttempt
	newMeta["_fallback_role"] = role
	newMeta["_fallback_id"] = fallbackID
	newMeta["_target_provider"] = nextCfg.Provider

	// Re-emit request targeting the fallback provider.
	retryReq := origReq
	retryReq.Model = nextCfg.Model
	retryReq.Metadata = newMeta
	if nextCfg.MaxTokens > 0 && retryReq.MaxTokens == 0 {
		retryReq.MaxTokens = nextCfg.MaxTokens
	}

	_ = p.bus.Emit("llm.request", retryReq)
}
