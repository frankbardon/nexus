// Package matcher is the staffing-match domain plugin. It lives
// under cmd/desktop/internal/matcher/ rather than plugins/<category>/
// because its types (candidateRecord, job ranking prompt, scoring
// rubric) are specific to one distributable application and would not
// be reusable from a different app. Go's internal/ mechanism enforces
// that boundary: no package outside cmd/desktop/ can import this one.
//
// FUTURE EXTRACTION NOTE (keep this comment when the second app
// appears):
//
// If a second app eventually needs the same "emit llm.request,
// wait for llm.response keyed by metadata" round-trip, do not
// extract this plugin as a generic ranking plugin. Instead, pull
// the round-trip plumbing (inflight map, metaKey constants,
// handleLLMResponse, the runMatch emit+select+timeout block)
// into a small helper type in a new package — something like:
//
//	pkg/llmhelp.RoundTripper{bus, source, role, timeout}
//	  .RoundTrip(ctx, messages) (events.LLMResponse, error)
//
// Each domain plugin instantiates one per plugin instance and
// calls RoundTrip from within its own handleDomainRequest. No
// new bus event types, no new plugin dependency, no lifecycle
// graph complications — just a helper. Everything else in this
// file (the candidate struct, the prompt, the parser, the
// scoring rubric, the JSON schema) stays in the app that owns
// it.
//
// Do not write pkg/llmhelp pre-emptively. Wait for the second
// real caller so you can shape the API around two concrete use
// sites instead of guessing.
package matcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// PluginID is the canonical identifier for the staffing-match app
// plugin. Exported so host binaries (cmd/desktop) can append it to
// eng.Config.Plugins.Active without stringly-typing it.
const PluginID = "nexus.app.staffingmatch"

// LLMProviderID names the LLM provider plugin this plugin depends
// on. Declared as a constant both so Dependencies() cannot drift
// from the actual string and so a future switch to a different
// provider is a single-point edit. The dependency is explicit and
// narrow: swapping providers is a future concern, and the correct
// refactor when that happens is a generic "any llm provider"
// dependency surface in Nexus core, not a second hardcoded string.
const LLMProviderID = "nexus.llm.anthropic"

// rankRole is the model registry role this plugin requests for
// ranking calls. The actual model ID and max_tokens come from the
// host binary's embedded config.yaml (see
// cmd/desktop/config-staffing.yaml), which lets you swap Haiku 4.5
// for Sonnet 4.6 or any other Anthropic model without recompiling
// the plugin. If the host binary does not define this role, the
// Anthropic provider falls back to the registry's default role.
const rankRole = "quick"

// rankTimeout bounds how long handleMatchRequest waits on
// llm.response before giving up and emitting an error. 90s is well
// above typical Haiku latencies (<5s for this payload) but below
// the human abandonment threshold, and leaves headroom for retry
// delays in the Anthropic provider's retry path.
const rankTimeout = 90 * time.Second

// metaKeySource tags the llm.request so the shared LLM provider's
// response can be routed back to this plugin specifically, not to
// some other subscriber on the same bus. The value matches the
// convention the dynamic planner uses.
const metaKeySource = "source"

// metaKeyRequestID carries the correlation ID through the
// request/response round-trip. Echoed back on LLMResponse.Metadata
// by the Anthropic provider unchanged.
const metaKeyRequestID = "request_id"

// Compile-time assertion that Plugin satisfies engine.Plugin.
var _ engine.Plugin = (*Plugin)(nil)

// Plugin is the staffing-match domain plugin. It owns the full
// job-description-to-ranked-candidates pipeline: candidate pool,
// rank prompt construction, LLM orchestration via the Nexus event
// bus, and JSON decoding of the model's response. For the first
// real slice every piece of that pipeline exists but is scoped:
// the candidate pool is hardcoded synthetic data, the LLM call is
// a single-shot sync request to Anthropic, and nothing reads PDFs.
//
// The plugin is deliberately unaware of Wails, of the webview, and
// of any specific UI. It receives MatchRequest events and emits
// MatchResult events. The cmd/desktop host binds these to
// its Wails frontend via bus Subscribe/Emit inside App-struct
// methods; the plugin itself never sees the webview.
type Plugin struct {
	bus       engine.EventBus
	logger    *slog.Logger
	unsubs    []func()
	outputDir string // configured output directory for writing match results

	// inflight routes incoming llm.response events back to the
	// handleMatchRequest goroutine that emitted the matching
	// llm.request. Keyed by the metaKeyRequestID value stored in
	// LLMRequest.Metadata (and echoed on LLMResponse.Metadata by
	// the provider). The channel is buffered so the bus dispatch
	// goroutine, which runs handleLLMResponse synchronously, is
	// never blocked waiting on a slow reader.
	mu       sync.Mutex
	inflight map[string]chan events.LLMResponse
}

// New constructs a fresh staffing-match plugin.
func New() engine.Plugin {
	return &Plugin{
		inflight: make(map[string]chan events.LLMResponse),
	}
}

func (p *Plugin) ID() string      { return PluginID }
func (p *Plugin) Name() string    { return "Staffing Match" }
func (p *Plugin) Version() string { return "0.1.0" }

// Dependencies locks this plugin to the Anthropic provider. The
// lifecycle manager uses this list to order Init calls, so the
// provider is guaranteed to be listening on llm.request before this
// plugin's Init returns. See the comment on LLMProviderID for why
// this is intentionally narrow.
func (p *Plugin) Dependencies() []string            { return []string{LLMProviderID} }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: EventMatchRequest, Priority: 50},
		// Priority 20 puts the handler earlier than the default
		// (50) because we want to claim the response before any
		// other subscriber — but we filter by metadata source
		// inside the handler so we never interfere with unrelated
		// llm.response traffic.
		{EventType: "llm.response", Priority: 20},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		EventMatchResult,
		"llm.request",
		"session.meta.title",
		"session.meta.preview",
		"session.meta.status",
		"session.file.created",
	}
}

// Init stores the bus handle and wires both subscriptions.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	if dir, ok := ctx.Config["output_dir"].(string); ok {
		p.outputDir = engine.ExpandPath(dir)
	}

	if p.inflight == nil {
		p.inflight = make(map[string]chan events.LLMResponse)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe(EventMatchRequest, p.handleMatchRequest,
			engine.WithSource(PluginID),
		),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithSource(PluginID),
			engine.WithPriority(20),
		),
	)

	p.logger.Info("staffing-match plugin initialized",
		"model_role", rankRole,
		"pool_size", len(candidatePool()))
	return nil
}

// Ready is a no-op: the plugin has no background goroutines, no
// network listeners, and nothing that needs to wait for sibling
// plugins to be up.
func (p *Plugin) Ready() error {
	return nil
}

// Shutdown unsubscribes from the bus.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil
	return nil
}

// handleMatchRequest is the real ranker. It builds the prompt,
// emits an llm.request with metadata-carrying correlation, blocks
// on the inflight channel for the response, parses the ranking,
// and emits match.result.
//
// The handler runs on a fresh goroutine so the bus dispatcher is
// never blocked on an LLM round-trip. The caller (host App) does
// not care whether the plugin handles synchronously or
// asynchronously — it is already waiting on its own channel for
// match.result.
func (p *Plugin) handleMatchRequest(e engine.Event[any]) {
	var req MatchRequest
	switch payload := e.Payload.(type) {
	case MatchRequest:
		req = payload
	case map[string]any:
		// Generic inbound path: the desktop shell's bus bridge
		// delivers frontend-originated events as map[string]any.
		req = MatchRequest{
			RequestID: strval(payload, "requestID"),
			JobText:   strval(payload, "jobText"),
			PDFPath:   strval(payload, "pdfPath"),
		}
		if topK, ok := payload["topK"].(float64); ok {
			req.TopK = int(topK)
		}
	default:
		p.logger.Warn("match.request received with unexpected payload type",
			"type", fmt.Sprintf("%T", e.Payload))
		return
	}

	go p.runMatch(req)
}

// strval extracts a string from a map, trying both the given key and
// common casing variants (camelCase keys from JSON).
func strval(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func (p *Plugin) runMatch(req MatchRequest) {
	p.logger.Info("match.request received",
		"request_id", req.RequestID,
		"job_text_len", len(req.JobText),
		"pdf_path", req.PDFPath,
		"top_k", req.TopK)

	// Resolve the job description. Precedence is documented in
	// MatchRequest: a non-empty JobText always wins, otherwise a
	// non-empty PDFPath triggers parse-then-rank, otherwise this
	// is an error. The "text wins" order means a user who has
	// both pasted text and attached a PDF gets their pasted text
	// (they almost certainly meant to override the PDF), and
	// future non-Wails callers that populate JobText from their
	// own sources do not need to worry about accidentally
	// triggering a PDF parse they did not ask for.
	jobText := req.JobText
	if jobText == "" && req.PDFPath != "" {
		extracted, err := extractPDFText(req.PDFPath)
		if err != nil {
			p.logger.Warn("PDF extraction failed",
				"request_id", req.RequestID,
				"pdf_path", req.PDFPath,
				"error", err)
			p.emitResult(MatchResult{
				RequestID: req.RequestID,
				Error:     fmt.Sprintf("reading PDF: %v", err),
			})
			return
		}
		p.logger.Info("PDF extracted",
			"request_id", req.RequestID,
			"extracted_chars", len(extracted))
		jobText = extracted
	}

	if jobText == "" {
		p.emitResult(MatchResult{
			RequestID: req.RequestID,
			Error:     "job description is empty (provide text or attach a PDF)",
		})
		return
	}

	// Every llm.request carries a per-call correlation ID distinct
	// from the MatchRequest.RequestID. We cannot reuse RequestID
	// directly because a single MatchRequest might eventually fan
	// out into multiple LLM calls (second-pass rerank, etc.) and
	// the inflight map needs to distinguish them.
	llmRequestID := fmt.Sprintf("%s-llm-1", req.RequestID)
	respCh := make(chan events.LLMResponse, 1)

	p.mu.Lock()
	p.inflight[llmRequestID] = respCh
	p.mu.Unlock()

	// Unregister on every exit path. defer is correct here because
	// the channel is buffered and the handler never blocks on a
	// send, so there is no deadlock risk from a late response
	// arriving after the receiver has given up.
	defer func() {
		p.mu.Lock()
		delete(p.inflight, llmRequestID)
		p.mu.Unlock()
	}()

	pool := candidatePool()
	llmReq := events.LLMRequest{SchemaVersion:
	// Role-based resolution: the Anthropic provider looks up
	// the actual model ID and max_tokens from the engine's
	// ModelRegistry, which the host binary populates from
	// core.models in config.yaml. Leaving Model and MaxTokens
	// zero-valued is the signal to use the role.
	events.LLMRequestVersion, Role: rankRole,
		Stream: false,
		Messages: []events.Message{
			{Role: "system", Content: rankSystemPrompt},
			{Role: "user", Content: rankUserMessage(jobText, pool)},
		},
		Metadata: map[string]any{
			metaKeySource:    PluginID,
			metaKeyRequestID: llmRequestID,
		},
	}

	// Latency is measured at the bus round-trip boundary: from the
	// moment we emit the llm.request to the moment the matching
	// llm.response lands on respCh. This includes Anthropic's
	// wall-clock time plus any Nexus-side retry backoff, which is
	// what the user actually experiences.
	startedAt := time.Now()

	if err := p.bus.Emit("llm.request", llmReq); err != nil {
		p.emitResult(MatchResult{
			RequestID: req.RequestID,
			Error:     fmt.Sprintf("emitting llm.request: %v", err),
		})
		return
	}

	select {
	case resp := <-respCh:
		latencyMs := time.Since(startedAt).Milliseconds()
		candidates, err := parseRanking(resp.Content, pool, req.TopK)
		if err != nil {
			p.logger.Error("failed to parse ranking response",
				"error", err,
				"content_preview", preview(resp.Content, 200))
			p.emitResult(MatchResult{
				RequestID: req.RequestID,
				Error:     fmt.Sprintf("parsing ranking: %v", err),
			})
			return
		}
		cost := buildCost(resp, latencyMs)
		p.logger.Info("match complete",
			"request_id", req.RequestID,
			"candidates", len(candidates),
			"prompt_tokens", cost.PromptTokens,
			"completion_tokens", cost.CompletionTokens,
			"usd", cost.USD,
			"latency_ms", cost.LatencyMs)
		result := MatchResult{
			RequestID:  req.RequestID,
			Candidates: candidates,
			Cost:       cost,
		}
		p.emitResult(result)
		p.emitSessionMeta(jobText, candidates)
		p.writeResultFile(result, jobText)
	case <-time.After(rankTimeout):
		p.emitResult(MatchResult{
			RequestID: req.RequestID,
			Error:     fmt.Sprintf("llm request timed out after %s", rankTimeout),
		})
	}
}

// handleLLMResponse routes llm.response events back to the
// goroutine that emitted the matching llm.request, keyed by the
// correlation ID we stashed in metadata. Unrelated llm.response
// traffic (from agents, planners, or any other caller) is silently
// ignored because its metadata["source"] will not match PluginID.
func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	if resp.Metadata == nil {
		return
	}
	source, _ := resp.Metadata[metaKeySource].(string)
	if source != PluginID {
		return
	}
	requestID, _ := resp.Metadata[metaKeyRequestID].(string)
	if requestID == "" {
		return
	}

	p.mu.Lock()
	ch, found := p.inflight[requestID]
	p.mu.Unlock()
	if !found {
		// Response arrived after the waiter gave up (timeout,
		// shutdown, etc.). Nothing to do.
		return
	}

	// Non-blocking send: channel is buffered-1 and the sender side
	// owns the only reader. If the slot is already full something
	// has gone very wrong (duplicate response), log and drop.
	select {
	case ch <- resp:
	default:
		p.logger.Warn("llm.response dropped, inflight channel already full",
			"request_id", requestID)
	}
}

// emitResult is a single-point emitter for match.result. Centralizing
// it keeps the error-path and success-path code in handleMatchRequest
// symmetric and ensures we never forget to emit on some exit path.
func (p *Plugin) emitResult(r MatchResult) {
	if err := p.bus.Emit(EventMatchResult, r); err != nil {
		p.logger.Error("failed to emit match.result", "error", err)
	}
}

// emitSessionMeta publishes session metadata events so the desktop
// shell can populate the session list with a meaningful title and
// preview for this run.
func (p *Plugin) emitSessionMeta(jobText string, candidates []Candidate) {
	// Title: first line of job text, truncated.
	title := firstLine(jobText, 60)
	if title == "" {
		title = "Untitled Match"
	}
	_ = p.bus.Emit("session.meta.title", map[string]any{
		"title": title,
	})

	// Preview: candidate count + top candidate name.
	preview := map[string]any{
		"candidateCount": len(candidates),
	}
	if len(candidates) > 0 {
		preview["topCandidate"] = candidates[0].Name
	}
	_ = p.bus.Emit("session.meta.preview", preview)

	_ = p.bus.Emit("session.meta.status", map[string]any{
		"status": "completed",
	})
}

// firstLine returns the first non-empty line of s, truncated to max runes.
func firstLine(s string, max int) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			s = s[:i]
			break
		}
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// preview returns the first n runes of s with an ellipsis if
// truncated. Used only for error logging — the full content would
// flood logs on parse failure, and the first few hundred characters
// are what you actually want to see in the terminal.
func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// writeResultFile writes a JSON summary of the match result to the
// configured output directory. If no output_dir is configured, this
// is a no-op. The file is named match-<timestamp>.json.
func (p *Plugin) writeResultFile(result MatchResult, jobText string) {
	if p.outputDir == "" {
		return
	}

	if err := os.MkdirAll(p.outputDir, 0o755); err != nil {
		p.logger.Warn("cannot create output dir", "error", err)
		return
	}

	summary := map[string]any{
		"request_id": result.RequestID,
		"job_text":   firstLine(jobText, 200),
		"candidates": result.Candidates,
		"cost":       result.Cost,
		"timestamp":  time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		p.logger.Warn("cannot marshal match result", "error", err)
		return
	}

	filename := fmt.Sprintf("match-%s.json", time.Now().Format("20060102-150405"))
	path := filepath.Join(p.outputDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.logger.Warn("cannot write match result file", "error", err, "path", path)
		return
	}

	p.logger.Info("match result written", "path", path)
	_ = p.bus.Emit("session.file.created", map[string]any{
		"path":   path,
		"action": "created",
	})
}
