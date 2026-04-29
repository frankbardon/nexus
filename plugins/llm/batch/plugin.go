// Package batch implements the cross-provider LLM Batch coordinator.
//
// One plugin, two providers, three event surfaces:
//
//   - Subscribe: "llm.batch.submit"  (events.BatchSubmit)
//   - Emit:      "llm.batch.status"  (events.BatchStatus)
//   - Emit:      "llm.batch.results" (events.BatchResults)
//
// The coordinator dispatches each submit to the configured provider's batch
// endpoint (Anthropic Messages Batches or OpenAI Batch API), persists state to
// disk so pollers survive restart, and polls in the background until the batch
// completes — at which point per-request results are emitted via the bus.
//
// v1 scope notes (intentionally explicit so future readers know what's not
// here):
//   - Direct-API auth only. Bedrock / Vertex / Azure modes are not plumbed
//     through; the per-provider auth code stays in providers/* and isn't
//     reused here. A second-caller refactor is the right time to extract.
//   - Text-only LLMRequest -> wire-format adapters. Multimodal, extended
//     thinking, prompt caching, predicted outputs, reasoning effort, and
//     citations are NOT carried into batched requests yet. The synchronous
//     llm.request path remains the supported way to use those features.
//   - Cross-provider batches (split N requests across providers) are out of
//     scope — one BatchSubmit = one provider.
//   - Cancellation is not exposed; pollers run until completion or process
//     exit.
package batch

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.llm.batch"
	pluginName = "LLM Batch Coordinator"
	version    = "0.1.0"

	// Canonical status vocabulary surfaced via BatchStatus.Status. Provider-
	// specific status strings are mapped onto these by the per-provider
	// status fetchers.
	statusSubmitted  = "submitted"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"
	statusCancelled  = "cancelled"

	defaultPollInterval   = 5 * time.Minute
	defaultDataDir        = "~/.nexus/batches"
	defaultBatchMaxTokens = 1024
	httpClientTimeout     = 5 * time.Minute
)

// Plugin is the batch coordinator. It owns:
//   - the bus subscription on "llm.batch.submit"
//   - the on-disk state directory
//   - the in-memory map of active batches and their pollers
//   - the per-provider auth credentials (api keys only in v1)
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	models  *engine.ModelRegistry

	pollInterval     time.Duration
	dataDir          string
	defaultMaxTokens int
	client           *http.Client

	// Direct-API credentials. Read from config (api_key / api_key_env) at
	// Init time. v1 deliberately doesn't reach into the anthropic/openai
	// provider plugins for credentials — config-driven keeps the plugin
	// independently activatable.
	anthropicAPIKey string
	openaiAPIKey    string

	// Test overrides. When non-empty these replace the production base URLs
	// — the per-provider helpers always check the override first, so unit
	// tests can point at httptest.Servers without touching production code
	// paths.
	anthropicBaseURL     string
	openaiFilesBaseURL   string
	openaiBatchesBaseURL string

	mu      sync.Mutex
	active  map[string]*activeBatch       // batch_id -> in-flight metadata
	pollers map[string]context.CancelFunc // batch_id -> poller cancel
	wg      sync.WaitGroup                // tracks running pollers for clean shutdown
	unsubs  []func()
}

// activeBatch is the in-memory record for one in-flight batch. It mirrors
// batchState (which is the persisted form) plus a few coordination fields.
type activeBatch struct {
	Provider     string
	BatchID      string
	SubmittedAt  time.Time
	OriginalReqs []events.BatchRequest
	Metadata     map[string]any
}

// New constructs a fresh Plugin instance.
func New() engine.Plugin { return &Plugin{} }

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.models = ctx.Models

	p.client = &http.Client{Timeout: httpClientTimeout}
	p.active = make(map[string]*activeBatch)
	p.pollers = make(map[string]context.CancelFunc)

	// Poll interval. Strings get parsed with time.ParseDuration so users can
	// write "5m", "30s", "1h" in YAML.
	p.pollInterval = defaultPollInterval
	if v, ok := ctx.Config["poll_interval"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("batch: invalid poll_interval %q: %w", v, err)
		}
		p.pollInterval = d
	}

	// Data directory for state files. Always tilde-expand before use.
	p.dataDir = engine.ExpandPath(defaultDataDir)
	if v, ok := ctx.Config["data_dir"].(string); ok && v != "" {
		p.dataDir = engine.ExpandPath(v)
	}
	if err := os.MkdirAll(p.dataDir, 0o755); err != nil {
		return fmt.Errorf("batch: create data dir %q: %w", p.dataDir, err)
	}

	// Default max_tokens applied when a batched LLMRequest didn't pin a
	// value itself. Anthropic requires the field; sending zero is rejected.
	p.defaultMaxTokens = defaultBatchMaxTokens
	switch v := ctx.Config["default_max_tokens"].(type) {
	case int:
		if v > 0 {
			p.defaultMaxTokens = v
		}
	case float64:
		if v > 0 {
			p.defaultMaxTokens = int(v)
		}
	}

	// Provider credentials. The shape mirrors the per-provider plugins for
	// muscle memory:
	//
	//   nexus.llm.batch:
	//     providers:
	//       anthropic:
	//         api_key: "sk-..."
	//         api_key_env: "ANTHROPIC_API_KEY"
	//       openai:
	//         api_key_env: "OPENAI_API_KEY"
	//
	// Either api_key (literal) or api_key_env (env-var indirection) wins;
	// missing both leaves the credential empty and the corresponding submit
	// path will fail loudly when called.
	if providers, ok := ctx.Config["providers"].(map[string]any); ok {
		if a, ok := providers["anthropic"].(map[string]any); ok {
			p.anthropicAPIKey = readAPIKey(a, "ANTHROPIC_API_KEY")
		}
		if o, ok := providers["openai"].(map[string]any); ok {
			p.openaiAPIKey = readAPIKey(o, "OPENAI_API_KEY")
		}
	}
	// Backwards-compat: also look at flat top-level keys, in case operators
	// prefer nexus.llm.batch.api_key_env (single-provider deployments).
	if p.anthropicAPIKey == "" {
		if v, ok := ctx.Config["anthropic_api_key_env"].(string); ok && v != "" {
			p.anthropicAPIKey = os.Getenv(v)
		}
	}
	if p.openaiAPIKey == "" {
		if v, ok := ctx.Config["openai_api_key_env"].(string); ok && v != "" {
			p.openaiAPIKey = os.Getenv(v)
		}
	}

	// Subscribe to submit events.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("llm.batch.submit", p.handleSubmit,
			engine.WithPriority(10),
			engine.WithSource(pluginID),
		),
	)

	// Resume any batches we left behind on a previous run.
	if err := p.resumePersistedBatches(); err != nil {
		// Resume failures are logged but non-fatal — the plugin should still
		// accept new submits even if the on-disk state is partially busted.
		p.logger.Warn("batch: resume persisted batches failed", "error", err)
	}

	p.logger.Info("batch coordinator ready",
		"poll_interval", p.pollInterval,
		"data_dir", p.dataDir,
		"anthropic_configured", p.anthropicAPIKey != "",
		"openai_configured", p.openaiAPIKey != "",
		"resumed", len(p.active),
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

// Shutdown unsubscribes and stops every poller. State files are intentionally
// left on disk so the next process boot resumes the same batches.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}

	p.mu.Lock()
	for id, cancel := range p.pollers {
		cancel()
		delete(p.pollers, id)
	}
	p.mu.Unlock()

	// Wait for pollers to exit (best-effort; bounded by ctx).
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "llm.batch.submit", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.batch.status",
		"llm.batch.results",
	}
}

// handleSubmit is the entrypoint for new batches.
func (p *Plugin) handleSubmit(event engine.Event[any]) {
	submit, ok := event.Payload.(events.BatchSubmit)
	if !ok {
		// Accept the pointer form too — bus emitters sometimes hand a *T to
		// the bus despite our convention. Cheap defensive cast.
		if ptr, ptrOK := event.Payload.(*events.BatchSubmit); ptrOK && ptr != nil {
			submit = *ptr
			ok = true
		}
	}
	if !ok {
		p.logger.Error("batch: invalid llm.batch.submit payload type", "type", fmt.Sprintf("%T", event.Payload))
		return
	}
	if err := p.submit(context.Background(), submit); err != nil {
		p.logger.Error("batch: submit failed", "provider", submit.Provider, "error", err)
	}
}

// submit dispatches one BatchSubmit to the right provider. Public-ish (lower-
// case but called directly in tests) so callers can drive the coordinator
// without going through the bus when the bus isn't relevant to the test.
func (p *Plugin) submit(ctx context.Context, sub events.BatchSubmit) error {
	if len(sub.Requests) == 0 {
		return fmt.Errorf("batch: no requests in submit payload")
	}
	var (
		batchID string
		err     error
	)
	switch sub.Provider {
	case "anthropic":
		batchID, err = p.submitAnthropic(ctx, sub.Requests)
	case "openai":
		batchID, err = p.submitOpenAI(ctx, sub.Requests)
	default:
		return fmt.Errorf("batch: unsupported provider %q (expected anthropic or openai)", sub.Provider)
	}
	if err != nil {
		return err
	}

	ab := &activeBatch{
		Provider:     sub.Provider,
		BatchID:      batchID,
		SubmittedAt:  time.Now().UTC(),
		OriginalReqs: sub.Requests,
		Metadata:     sub.Metadata,
	}
	if err := saveBatch(p.dataDir, &batchState{
		Provider:     ab.Provider,
		BatchID:      ab.BatchID,
		SubmittedAt:  ab.SubmittedAt,
		OriginalReqs: ab.OriginalReqs,
		Metadata:     ab.Metadata,
	}); err != nil {
		// Persist failure is non-fatal but loud — the poller still runs in
		// memory. Operator just loses restart resilience for this batch.
		p.logger.Error("batch: persist state failed", "batch_id", batchID, "error", err)
	}

	p.startPoller(ab)

	_ = p.bus.Emit("llm.batch.status", events.BatchStatus{
		Provider: ab.Provider,
		BatchID:  ab.BatchID,
		Status:   statusSubmitted,
		Counts:   events.BatchCounts{Total: len(ab.OriginalReqs)},
	})
	p.logger.Info("batch submitted", "provider", sub.Provider, "batch_id", batchID, "requests", len(sub.Requests))
	return nil
}

// startPoller registers ab in the active map and launches its poller goroutine.
// Caller must NOT hold p.mu.
func (p *Plugin) startPoller(ab *activeBatch) {
	pctx, cancel := context.WithCancel(context.Background())

	p.mu.Lock()
	p.active[ab.BatchID] = ab
	p.pollers[ab.BatchID] = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.pollLoop(pctx, ab)
	}()
}

// pollLoop runs until the batch completes, fails, is cancelled, or the
// process shuts down. Ticks at p.pollInterval; emits a BatchStatus on each
// tick so observers can chart progress without subscribing to provider-
// specific events.
func (p *Plugin) pollLoop(ctx context.Context, ab *activeBatch) {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	// Run one poll immediately so tests (and impatient operators) don't have
	// to wait a full interval before seeing any progress signal.
	for {
		if p.pollOnce(ctx, ab) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollOnce performs a single status check + (on completion) results fetch.
// Returns true when the batch is in a terminal state and the poller should
// exit.
func (p *Plugin) pollOnce(ctx context.Context, ab *activeBatch) bool {
	switch ab.Provider {
	case "anthropic":
		return p.pollOnceAnthropic(ctx, ab)
	case "openai":
		return p.pollOnceOpenAI(ctx, ab)
	default:
		p.logger.Error("batch: pollOnce called with unsupported provider", "provider", ab.Provider)
		return true
	}
}

func (p *Plugin) pollOnceAnthropic(ctx context.Context, ab *activeBatch) bool {
	status, counts, err := p.statusAnthropic(ctx, ab.BatchID)
	if err != nil {
		p.logger.Warn("batch: anthropic status fetch failed", "batch_id", ab.BatchID, "error", err)
		return false
	}
	_ = p.bus.Emit("llm.batch.status", events.BatchStatus{
		Provider: ab.Provider,
		BatchID:  ab.BatchID,
		Status:   status,
		Counts:   counts,
	})
	switch status {
	case statusCompleted, statusFailed, statusCancelled:
		results, err := p.resultsAnthropic(ctx, ab.BatchID)
		if err != nil {
			p.logger.Error("batch: anthropic results fetch failed", "batch_id", ab.BatchID, "error", err)
			// Stop polling regardless — we'll have left the state file alone
			// so the operator can investigate and re-poll out-of-band.
			return true
		}
		p.finalize(ab, results)
		return true
	}
	return false
}

func (p *Plugin) pollOnceOpenAI(ctx context.Context, ab *activeBatch) bool {
	status, counts, outputFileID, errorFileID, err := p.statusOpenAI(ctx, ab.BatchID)
	if err != nil {
		p.logger.Warn("batch: openai status fetch failed", "batch_id", ab.BatchID, "error", err)
		return false
	}
	_ = p.bus.Emit("llm.batch.status", events.BatchStatus{
		Provider: ab.Provider,
		BatchID:  ab.BatchID,
		Status:   status,
		Counts:   counts,
	})
	switch status {
	case statusCompleted, statusFailed, statusCancelled:
		// Some terminal states have no output_file_id (e.g. fully-failed
		// batches with only an error_file_id). Try output first, error file
		// second, then fall through with whatever errors we collected.
		var (
			results  []events.BatchResult
			fetchErr error
		)
		if outputFileID != "" {
			results, fetchErr = p.resultsOpenAI(ctx, outputFileID)
		} else if errorFileID != "" {
			results, fetchErr = p.resultsOpenAI(ctx, errorFileID)
		} else {
			fetchErr = fmt.Errorf("no output_file_id or error_file_id in terminal status")
		}
		if fetchErr != nil {
			p.logger.Error("batch: openai results fetch failed", "batch_id", ab.BatchID, "error", fetchErr)
			return true
		}
		p.finalize(ab, results)
		return true
	}
	return false
}

// finalize emits the BatchResults event and tears down state for one batch.
func (p *Plugin) finalize(ab *activeBatch, results []events.BatchResult) {
	_ = p.bus.Emit("llm.batch.results", events.BatchResults{
		Provider: ab.Provider,
		BatchID:  ab.BatchID,
		Results:  results,
	})
	if err := deleteBatch(p.dataDir, ab.BatchID); err != nil {
		p.logger.Warn("batch: delete state file failed", "batch_id", ab.BatchID, "error", err)
	}

	p.mu.Lock()
	delete(p.active, ab.BatchID)
	if cancel, ok := p.pollers[ab.BatchID]; ok {
		// Don't actually call cancel here — the poller loop returning will
		// release the goroutine. Just clear the map entry.
		_ = cancel
		delete(p.pollers, ab.BatchID)
	}
	p.mu.Unlock()

	p.logger.Info("batch finalized", "provider", ab.Provider, "batch_id", ab.BatchID, "results", len(results))
}

// resumePersistedBatches loads every state file from p.dataDir and restarts a
// poller for each. Called at Init.
func (p *Plugin) resumePersistedBatches() error {
	states, err := loadBatches(p.dataDir)
	if err != nil {
		return err
	}
	for _, s := range states {
		// Only resume providers we have credentials for — running a poller
		// against an empty key would just spam 401 errors.
		switch s.Provider {
		case "anthropic":
			if p.anthropicAPIKey == "" {
				p.logger.Warn("batch: cannot resume — no anthropic api key", "batch_id", s.BatchID)
				continue
			}
		case "openai":
			if p.openaiAPIKey == "" {
				p.logger.Warn("batch: cannot resume — no openai api key", "batch_id", s.BatchID)
				continue
			}
		default:
			p.logger.Warn("batch: cannot resume — unknown provider", "provider", s.Provider, "batch_id", s.BatchID)
			continue
		}
		ab := &activeBatch{
			Provider:     s.Provider,
			BatchID:      s.BatchID,
			SubmittedAt:  s.SubmittedAt,
			OriginalReqs: s.OriginalReqs,
			Metadata:     s.Metadata,
		}
		p.startPoller(ab)
	}
	return nil
}

// readAPIKey extracts an API key from one provider's config block. Both
// "api_key" (literal) and "api_key_env" (env-var indirection) are supported;
// literal wins when both are set.
func readAPIKey(cfg map[string]any, fallbackEnv string) string {
	if v, ok := cfg["api_key"].(string); ok && v != "" {
		return v
	}
	envVar, _ := cfg["api_key_env"].(string)
	if envVar == "" {
		envVar = fallbackEnv
	}
	if envVar != "" {
		return os.Getenv(envVar)
	}
	return ""
}
