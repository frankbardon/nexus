// Package classifier implements an LLM-judge per-step model router.
//
// The classifier sees the user's most recent message, asks a small fast
// model to rank the difficulty among the configured candidates, and
// caches the answer keyed by a prompt-prefix hash so repeat prompts
// don't re-pay the classifier latency budget.
//
// Routing happens synchronously on before:llm.request:
//   - Cache hit: rewrite Model immediately. Zero added latency.
//   - Cache miss: leave Model unchanged (default routing), and spawn a
//     background goroutine that classifies via the LLM and warms the
//     cache for the next call. This avoids paying the classifier
//     round-trip on the very first request — the trade-off documented
//     in the idea 09 plan ("first step in a chain pays cache creation").
//
// Recursion guard: the classifier's own llm.request is tagged
// `_source: nexus.router.classifier` and the handler skips when that
// tag is set, so the classifier cannot route itself.
package classifier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.router.classifier"

const defaultClassifierPrompt = `You are a routing classifier. Given the user prompt below, decide which model tier should answer.

Respond with EXACTLY one word from this list and nothing else: %s

The candidates are ordered cheapest first. Pick the cheapest tier that can answer the prompt correctly. Use a stronger tier only when the prompt requires multi-step reasoning, complex tool use, long-context analysis, or nuanced creative output.

Prompt:
%s
`

const (
	defaultPrefixChars  = 256
	defaultLatencyMs    = 800
	defaultCacheEntries = 1024
)

// New creates a new classifier router instance.
func New() engine.Plugin {
	return &Plugin{
		cache: newCache(defaultCacheEntries),
	}
}

// Plugin implements the LLM-classifier router.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	classifierModel string
	classifierRole  string
	candidates      []string
	fallback        string
	prefixChars     int
	latencyMs       int
	prompt          string
	cacheEnabled    bool

	cache *cache

	mu      sync.Mutex
	pending map[string]chan events.LLMResponse // call id -> classification response
	unsubs  []func()
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Classifier Router" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.pending = make(map[string]chan events.LLMResponse)

	if v, ok := ctx.Config["classifier_model"].(string); ok {
		p.classifierModel = v
	}
	if v, ok := ctx.Config["classifier_role"].(string); ok {
		p.classifierRole = v
	}
	if v, ok := ctx.Config["fallback"].(string); ok {
		p.fallback = v
	}
	if rawCands, ok := ctx.Config["candidates"].([]any); ok {
		for _, c := range rawCands {
			if s, ok := c.(string); ok && s != "" {
				p.candidates = append(p.candidates, s)
			}
		}
	}
	if len(p.candidates) == 0 {
		return fmt.Errorf("router.classifier: candidates list is required")
	}
	if p.classifierModel == "" && p.classifierRole == "" {
		return fmt.Errorf("router.classifier: either classifier_model or classifier_role is required")
	}

	p.prefixChars = defaultPrefixChars
	if v, ok := numericInt(ctx.Config["prefix_chars"]); ok && v > 0 {
		p.prefixChars = v
	}
	p.latencyMs = defaultLatencyMs
	if v, ok := numericInt(ctx.Config["latency_budget_ms"]); ok && v > 0 {
		p.latencyMs = v
	}
	p.prompt = defaultClassifierPrompt
	if v, ok := ctx.Config["prompt"].(string); ok && v != "" {
		p.prompt = v
	}
	p.cacheEnabled = true
	if v, ok := ctx.Config["cache_classification"].(bool); ok {
		p.cacheEnabled = v
	}
	cacheCap := defaultCacheEntries
	if v, ok := numericInt(ctx.Config["cache_max_entries"]); ok && v > 0 {
		cacheCap = v
	}
	p.cache = newCache(cacheCap)

	// Priority 50 lines up with the metadata router. If both are active the
	// metadata router runs first (deterministic), and the classifier only
	// fires when no metadata rule already rewrote the model. We detect that
	// via the _routed_by tag the metadata router sets.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(45), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithPriority(0), engine.WithSource(pluginID)),
	)

	p.logger.Info("classifier router initialized",
		"classifier_model", p.classifierModel,
		"classifier_role", p.classifierRole,
		"candidates", strings.Join(p.candidates, ","),
		"fallback", p.fallback,
		"cache_enabled", p.cacheEnabled,
		"latency_budget_ms", p.latencyMs)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.mu.Lock()
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:llm.request", Priority: 45},
		{EventType: "llm.response", Priority: 0},
	}
}

func (p *Plugin) Emissions() []string { return []string{"llm.request"} }

func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Don't route classifier's own probe (recursion guard) or other meta
	// internal requests (planner, compaction, etc) — those are categorical
	// not user-driven. Metadata router already covers them.
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}
	// Already routed by the metadata router — defer to its decision.
	if _, routed := req.Metadata["_routed_by"]; routed {
		return
	}
	// Specific provider pinned (fallback retry) — leave alone.
	if _, pinned := req.Metadata["_target_provider"].(string); pinned {
		return
	}

	prompt := userPrompt(req)
	if prompt == "" {
		return
	}
	key := promptHash(prompt, p.prefixChars)

	if p.cacheEnabled {
		if model, ok := p.cache.get(key); ok {
			p.applyDecision(req, model, "cache")
			return
		}
	}

	// Cache miss: route to fallback now and warm the cache asynchronously.
	if p.fallback != "" {
		p.applyDecision(req, p.fallback, "fallback")
	}
	go p.classifyAndCache(prompt, key)
}

// applyDecision rewrites the request and records the routing trail.
func (p *Plugin) applyDecision(req *events.LLMRequest, model, why string) {
	if model == "" || model == req.Model {
		return
	}
	prev := req.Model
	req.Model = model
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["_routed_by"] = pluginID
	req.Metadata["_routed_reason"] = why
	if prev != "" {
		req.Metadata["_routed_from_model"] = prev
	}
	p.logger.Debug("classifier routed", "reason", why, "from", prev, "to", model)
}

// classifyAndCache emits a classification request, awaits the response
// within the latency budget, parses the chosen tier from the response,
// and stores it in the cache for future lookups.
func (p *Plugin) classifyAndCache(prompt, key string) {
	callID := newCallID()
	ch := make(chan events.LLMResponse, 1)

	p.mu.Lock()
	p.pending[callID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, callID)
		p.mu.Unlock()
	}()

	probe := events.LLMRequest{
		Role:  p.classifierRole,
		Model: p.classifierModel,
		Messages: []events.Message{
			{Role: "user", Content: fmt.Sprintf(p.prompt, strings.Join(p.candidates, ", "), prompt)},
		},
		MaxTokens: 32,
		Metadata: map[string]any{
			"_source":    pluginID,
			"_call_id":   callID,
			"task_kind":  "classify",
			"_routed_by": pluginID, // suppress further routing on this probe
		},
	}
	if veto, err := p.bus.EmitVetoable("before:llm.request", &probe); err == nil && veto.Vetoed {
		p.logger.Debug("classifier probe vetoed, skipping", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", probe)

	select {
	case resp, ok := <-ch:
		if !ok {
			return
		}
		choice := resolveChoice(resp.Content, p.candidates)
		if choice == "" {
			p.logger.Debug("classifier returned unparseable choice", "content", resp.Content)
			return
		}
		if p.cacheEnabled {
			p.cache.put(key, choice)
		}
	case <-time.After(time.Duration(p.latencyMs) * time.Millisecond):
		p.logger.Debug("classifier timeout, cache not warmed", "key", key)
	}
}

func (p *Plugin) handleLLMResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	callID, _ := resp.Metadata["_call_id"].(string)
	if callID == "" {
		return
	}
	p.mu.Lock()
	ch, ok := p.pending[callID]
	p.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// userPrompt extracts the most recent user-role message content.
func userPrompt(req *events.LLMRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role == "user" && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

// promptHash returns a stable cache key for the first n characters of the
// prompt. Truncation is what makes the cache useful — most multi-turn
// conversations share a long shared prefix and only diverge near the end.
func promptHash(prompt string, n int) string {
	if n > 0 && len(prompt) > n {
		prompt = prompt[:n]
	}
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:16])
}

// resolveChoice maps the classifier's free-text answer to one of the
// candidate model ids. Falls back to substring matching since small models
// sometimes return verbose answers despite the "EXACTLY one word" prompt.
func resolveChoice(text string, candidates []string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	for _, c := range candidates {
		if t == c {
			return c
		}
	}
	for _, c := range candidates {
		if strings.Contains(t, c) {
			return c
		}
	}
	return ""
}

func numericInt(v any) (int, bool) {
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

func newCallID() string {
	var b [12]byte
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> uint(i*4))
	}
	return hex.EncodeToString(b[:])
}
