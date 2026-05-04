// Package hitlsynthesizer renders context-aware approval prompts for
// HITL requests via a small/cheap LLM. Other plugins opt their
// hitl.requested events in by setting PromptSynthesizer to this
// plugin's capability ID ("hitl.prompt_synthesizer"). When the request
// arrives with no Prompt set, the synthesizer assembles a brief
// system+user prompt around the action kind and reference, asks the
// configured model to produce a single approval question, and writes
// the answer back into req.Prompt before IO plugins render it.
//
// The synthesizer subscribes to two channels:
//
//   - before:hitl.requested — canonical, vetoable, pointer-payload entry
//     point. Every in-tree HITL emitter (nexus.control.hitl ask_user,
//     nexus.gate.approval_policy, the shared memory approval helper)
//     calls EmitVetoable on this event before publishing the value-shape
//     hitl.requested so the synthesizer can mutate req.Prompt in place.
//     The bus guarantees handlers see the same *VetoablePayload pointer.
//
//   - hitl.requested — non-vetoable. Kept as a backward-compat fallback
//     for out-of-tree emitters that publish a *HITLRequest pointer
//     directly. Value payloads are skipped (a copy can't carry mutations
//     back); all in-tree value emissions follow the before: path first
//     so by the time IO plugins see the value form, Prompt is populated.
//
// Synthesised prompts are cached on disk (write-through JSONL) keyed by
// (ActionKind, hash(ActionRef)) so identical actions don't pay the LLM
// cost twice within a session.
package hitlsynthesizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.control.hitl_synthesizer"
	pluginName = "HITL Prompt Synthesizer"
	version    = "0.1.0"

	// CapabilityName is the dotted capability ID this plugin advertises.
	// Other plugins set HITLRequest.PromptSynthesizer to this value to
	// opt into LLM-rendered prompts.
	CapabilityName = "hitl.prompt_synthesizer"

	// llmSource tags llm.request emissions so handleLLMResponse can
	// filter responses meant for this plugin and ignore unrelated
	// traffic.
	llmSource = pluginID

	defaultMaxActionRefChars = 1500
	defaultFallbackTemplate  = "Approve action: {{.action_kind}}"

	defaultSystemPrompt = "You write short, concrete approval prompts for a human reviewer. " +
		"Given an action description, produce a single sentence ending in a question mark. " +
		"Do not preamble. Do not explain. Do not include the action verbatim. " +
		"Mention the most important detail (path, command, namespace, etc.) so the reviewer " +
		"can decide quickly. Output the prompt and nothing else."
)

// Plugin renders HITL prompts via a small LLM and caches the results.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	// Configuration.
	model              string
	modelRole          string
	maxActionRefChars  int
	cacheEnabled       bool
	fallbackTemplate   *template.Template
	fallbackTemplateRaw string

	// Cache (in-memory mirror of cache.jsonl).
	cacheMu sync.RWMutex
	cache   map[string]string

	// pending tracks in-flight llm.request → llm.response correlations.
	// Keyed by the synthesis correlation ID emitted in
	// LLMRequest.Metadata["_synth_id"].
	pendingMu sync.Mutex
	pending   map[string]chan string

	corrCounter uint64

	unsubs []func()
}

// New creates the synthesizer plugin.
func New() engine.Plugin {
	return &Plugin{
		cache:   make(map[string]string),
		pending: make(map[string]chan string),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement {
	return nil
}

// Capabilities advertises the prompt-synthesizer capability so other
// plugins can name it via HITLRequest.PromptSynthesizer.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        CapabilityName,
			Description: "LLM-rendered HITL approval prompts derived from action kind and reference.",
		},
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	p.maxActionRefChars = defaultMaxActionRefChars
	p.cacheEnabled = true
	p.fallbackTemplateRaw = defaultFallbackTemplate

	if v, ok := ctx.Config["model"].(string); ok && v != "" {
		// "model" can be a model role (resolved via core.models) or a
		// raw model ID. The provider plugins handle either via
		// LLMRequest.Role / LLMRequest.Model.
		p.modelRole = v
	}
	if v, ok := ctx.Config["model_id"].(string); ok && v != "" {
		// Optional override: explicit model ID, bypassing role lookup.
		p.model = v
	}
	if v, ok := ctx.Config["max_action_ref_chars"].(int); ok && v > 0 {
		p.maxActionRefChars = v
	} else if v, ok := ctx.Config["max_action_ref_chars"].(float64); ok && v > 0 {
		p.maxActionRefChars = int(v)
	}
	if v, ok := ctx.Config["cache_enabled"].(bool); ok {
		p.cacheEnabled = v
	}
	if v, ok := ctx.Config["fallback_prompt"].(string); ok && v != "" {
		p.fallbackTemplateRaw = v
	}

	tmpl, err := template.New("fallback").Parse(p.fallbackTemplateRaw)
	if err != nil {
		return fmt.Errorf("hitl_synthesizer: parse fallback_prompt: %w", err)
	}
	p.fallbackTemplate = tmpl

	if p.modelRole == "" && p.model == "" {
		p.modelRole = "haiku"
	}

	if err := p.loadCache(); err != nil {
		// Non-fatal — start with an empty cache.
		p.logger.Warn("hitl_synthesizer: cache load failed", "error", err)
	}

	p.unsubs = append(p.unsubs,
		// Run before all IO plugins (priority 0/10/50) so we can mutate
		// the request before they render it. WithPriority is signed; lower
		// runs earlier. -100 keeps us comfortably ahead of every IO
		// subscriber currently in tree.
		p.bus.Subscribe("hitl.requested", p.handleHITLRequested,
			engine.WithPriority(-100), engine.WithSource(pluginID)),
		// before:hitl.requested is the proper vetoable path for future
		// emitters; we never veto, only mutate.
		p.bus.Subscribe("before:hitl.requested", p.handleBeforeHITLRequested,
			engine.WithPriority(-100), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("hitl prompt synthesizer initialized",
		"model_role", p.modelRole,
		"model", p.model,
		"max_action_ref_chars", p.maxActionRefChars,
		"cache_enabled", p.cacheEnabled,
		"cache_entries", len(p.cache),
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
		{EventType: "hitl.requested", Priority: -100},
		{EventType: "before:hitl.requested", Priority: -100},
		{EventType: "llm.response", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
	}
}

// ── Event Handlers ──────────────────────────────────────────────────────

// handleHITLRequested intercepts hitl.requested. The bus delivers a
// shared payload to subsequent subscribers in priority order; if the
// emitter passed *events.HITLRequest, we can mutate Prompt in place
// and the IO plugins (priority 0–50) will see the rendered text. If
// the emitter passed events.HITLRequest by value, the payload is a
// copy — we can't propagate, so we skip (the request will arrive at
// IO with whatever Prompt the emitter set, or empty).
func (p *Plugin) handleHITLRequested(event engine.Event[any]) {
	req, ok := event.Payload.(*events.HITLRequest)
	if !ok {
		// Value payloads can't carry mutations forward; skip.
		return
	}
	p.maybeSynthesize(req)
}

// handleBeforeHITLRequested handles the vetoable before:hitl.requested
// pattern. The bus wraps payloads in *engine.VetoablePayload, so we
// dig out the original first.
func (p *Plugin) handleBeforeHITLRequested(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.HITLRequest)
	if !ok {
		return
	}
	p.maybeSynthesize(req)
}

// maybeSynthesize is the shared core: validate the opt-in conditions,
// consult the cache, fall back to the LLM, and finally write the
// rendered prompt onto req.
func (p *Plugin) maybeSynthesize(req *events.HITLRequest) {
	if req == nil {
		return
	}
	if req.PromptSynthesizer != CapabilityName {
		return
	}
	if req.Prompt != "" {
		// Already rendered — nothing to do.
		return
	}

	cacheKey := buildCacheKey(req.ActionKind, req.ActionRef)
	if p.cacheEnabled {
		if hit, ok := p.cacheGet(cacheKey); ok {
			req.Prompt = hit
			p.logger.Debug("hitl_synthesizer: cache hit",
				"action_kind", req.ActionKind, "request_id", req.ID)
			return
		}
	}

	prompt, err := p.synthesize(req)
	if err != nil {
		p.logger.Warn("hitl_synthesizer: synthesis failed, using fallback",
			"error", err, "action_kind", req.ActionKind, "request_id", req.ID)
		prompt = p.renderFallback(req)
	}
	req.Prompt = prompt

	if p.cacheEnabled && err == nil {
		p.cachePut(cacheKey, prompt, req.ActionKind)
	}
}

// synthesize emits llm.request and returns the rendered prompt. Blocks
// the dispatch goroutine until the provider finishes (synchronous bus
// dispatch makes this safe — the provider emits llm.response from
// inside the same call).
func (p *Plugin) synthesize(req *events.HITLRequest) (string, error) {
	corrID := p.nextCorrID(req.ID)

	respCh := make(chan string, 1)
	p.pendingMu.Lock()
	p.pending[corrID] = respCh
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, corrID)
		p.pendingMu.Unlock()
	}()

	systemMsg := defaultSystemPrompt
	userMsg := buildUserPrompt(req, p.maxActionRefChars)

	emitErr := p.bus.Emit("llm.request", events.LLMRequest{
		Role:  p.modelRole,
		Model: p.model,
		Messages: []events.Message{
			{Role: "system", Content: systemMsg},
			{Role: "user", Content: userMsg},
		},
		Stream: false,
		Metadata: map[string]any{
			"_source":   llmSource,
			"_synth_id": corrID,
			"task_kind": "hitl_synthesize",
		},
		Tags: map[string]string{"source_plugin": pluginID},
	})
	if emitErr != nil {
		return "", fmt.Errorf("emit llm.request: %w", emitErr)
	}

	select {
	case content := <-respCh:
		content = strings.TrimSpace(content)
		if content == "" {
			return "", errors.New("synthesizer received empty response")
		}
		return content, nil
	default:
		// Provider didn't emit llm.response (likely core.error path).
		return "", errors.New("synthesizer received no response")
	}
}

// handleLLMResponse routes responses tagged with our llmSource back to
// the synthesize() call that's blocked waiting for them.
func (p *Plugin) handleLLMResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	source, _ := resp.Metadata["_source"].(string)
	if source != llmSource {
		return
	}
	corrID, _ := resp.Metadata["_synth_id"].(string)
	if corrID == "" {
		return
	}
	p.pendingMu.Lock()
	ch, exists := p.pending[corrID]
	p.pendingMu.Unlock()
	if !exists {
		return
	}
	select {
	case ch <- resp.Content:
	default:
		p.logger.Warn("hitl_synthesizer: response dropped — channel full", "synth_id", corrID)
	}
}

// renderFallback applies the configured fallback_prompt template to a
// minimal context derived from the request. Errors fall back to the
// raw template string.
func (p *Plugin) renderFallback(req *events.HITLRequest) string {
	if p.fallbackTemplate == nil {
		return p.fallbackTemplateRaw
	}
	data := map[string]any{
		"action_kind":      req.ActionKind,
		"action_ref":       req.ActionRef,
		"requester_plugin": req.RequesterPlugin,
		"request_id":       req.ID,
	}
	var buf bytes.Buffer
	if err := p.fallbackTemplate.Execute(&buf, data); err != nil {
		p.logger.Warn("hitl_synthesizer: fallback template execute failed",
			"error", err, "action_kind", req.ActionKind)
		return p.fallbackTemplateRaw
	}
	return buf.String()
}

// nextCorrID returns a synthesis-scoped correlation ID. Includes the
// HITL request ID for log readability and a monotonically increasing
// counter to disambiguate identical IDs (defensive — request IDs are
// unique in practice).
func (p *Plugin) nextCorrID(reqID string) string {
	p.pendingMu.Lock()
	p.corrCounter++
	n := p.corrCounter
	p.pendingMu.Unlock()
	if reqID == "" {
		return fmt.Sprintf("synth-%d", n)
	}
	return fmt.Sprintf("synth-%s-%d", reqID, n)
}

// ── User-prompt assembly ────────────────────────────────────────────────

// buildUserPrompt renders the user-facing instruction sent to the
// model. ActionRef is JSON-encoded then truncated to maxChars to bound
// token cost on pathological payloads.
func buildUserPrompt(req *events.HITLRequest, maxChars int) string {
	var sb strings.Builder

	sb.WriteString("<action_kind>")
	sb.WriteString(req.ActionKind)
	sb.WriteString("</action_kind>\n")

	if req.RequesterPlugin != "" {
		sb.WriteString("<requester_plugin>")
		sb.WriteString(req.RequesterPlugin)
		sb.WriteString("</requester_plugin>\n")
	}

	if len(req.ActionRef) > 0 {
		actionJSON, err := json.MarshalIndent(req.ActionRef, "", "  ")
		if err != nil {
			actionJSON = []byte(fmt.Sprintf("%v", req.ActionRef))
		}
		ref := string(actionJSON)
		if maxChars > 0 && len(ref) > maxChars {
			ref = ref[:maxChars] + "\n…(truncated)"
		}
		sb.WriteString("<action_ref>\n")
		sb.WriteString(ref)
		sb.WriteString("\n</action_ref>\n")
	}

	if req.PromptTemplate != "" {
		sb.WriteString("<prompt_template>\n")
		sb.WriteString(req.PromptTemplate)
		sb.WriteString("\n</prompt_template>\n")
	}

	sb.WriteString("\nWrite a single approval question for the reviewer. ")
	sb.WriteString("Mention the most important concrete detail. End with a question mark.")
	return sb.String()
}
