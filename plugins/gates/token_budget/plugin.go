// Package tokenbudget implements a multi-dimensional token / cost budget
// gate.
//
// Ceilings can be set independently across three dimensions:
//
//   - session:       per current session (in-memory; resets at session end)
//   - tenant:        per Tags["tenant"] value (persisted via app-scope SQLite)
//   - source_plugin: per Tags["source_plugin"] value (per-session)
//
// Each ceiling can limit input tokens, output tokens, total tokens, or USD
// cost — and tenant ceilings can also be windowed to a rolling 24h
// ("max_usd_per_day"). On exceed, the gate either blocks (vetoes), warns,
// or downgrades the model (rewrites Model to the cheapest entry from a
// configured candidate list, computed against pkg/engine/pricing).
//
// Backward compatibility: the legacy top-level `max_tokens` config key is
// honored as a single session-total-tokens ceiling.
package tokenbudget

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.token_budget"

// Dimension names. Tenants and source_plugin keys come from req.Tags;
// session is the implicit current-session bucket.
const (
	dimSession      = "session"
	dimTenant       = "tenant"
	dimSourcePlugin = "source_plugin"
)

// On-exceed actions.
const (
	actionBlock     = "block"
	actionWarn      = "warn"
	actionDowngrade = "downgrade-model"
)

// Window types for ceilings. session/global windows are unbounded;
// "day" rolls every UTC midnight.
const (
	windowSession = "session"
	windowDay     = "day"
)

// New creates a new token budget gate plugin instance.
func New() engine.Plugin {
	return &Plugin{}
}

// Plugin gates before:llm.request and accumulates spend on llm.response.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	store   storage.Storage

	ceilings            []ceiling
	downgradeCandidates []string
	pricing             *pricing.Table
	warningEmitted      map[string]bool

	// in-memory accumulators for session-scoped buckets:
	// key = dimension|bucketKey -> totals.
	mu     sync.Mutex
	totals map[string]*usageTotals

	unsubs []func()
}

// ceiling holds one limit. Exactly one of MaxInputTokens / MaxOutputTokens /
// MaxTotalTokens / MaxUSD must be > 0 (multiple are allowed and ANDed —
// any exceeded triggers OnExceed).
type ceiling struct {
	Dimension string // session | tenant | source_plugin
	Window    string // session | day; defaults to session
	Match     string // optional bucket-key restriction (e.g. "tools.web" for source_plugin)
	OnExceed  string // block | warn | downgrade-model
	MaxInput  int
	MaxOutput int
	MaxTotal  int
	MaxUSD    float64
	Message   string
}

// usageTotals is the running spend for a single bucket.
type usageTotals struct {
	WindowStart      time.Time
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	CostUSD          float64
	WarningEmittedAt float64 // utilisation pct at which we last warned (0–1)
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Token Budget Gate" }
func (p *Plugin) Version() string                   { return "0.2.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.totals = make(map[string]*usageTotals)
	p.warningEmitted = make(map[string]bool)
	p.pricing = mergedPricing(ctx.Config)

	ceilings, err := parseCeilings(ctx.Config)
	if err != nil {
		return fmt.Errorf("token_budget: %w", err)
	}
	p.ceilings = ceilings

	if rawCands, ok := ctx.Config["downgrade_candidates"].([]any); ok {
		for _, c := range rawCands {
			if s, ok := c.(string); ok && s != "" {
				p.downgradeCandidates = append(p.downgradeCandidates, s)
			}
		}
	}

	// Tenant ceilings cross sessions, so they need durable storage.
	if anyTenantCeiling(p.ceilings) && ctx.Storage != nil {
		s, err := ctx.Storage(storage.ScopeApp)
		if err != nil {
			return fmt.Errorf("token_budget: open app storage: %w", err)
		}
		if err := initTenantSchema(s.DB()); err != nil {
			return fmt.Errorf("token_budget: init schema: %w", err)
		}
		p.store = s
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithPriority(0), engine.WithSource(pluginID)),
	)

	p.logger.Info("token budget gate initialized",
		"ceilings", len(p.ceilings),
		"downgrade_candidates", len(p.downgradeCandidates),
		"persistence", p.store != nil)
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
		{EventType: "before:llm.request", Priority: 10},
		{EventType: "llm.response", Priority: 0},
	}
}

func (p *Plugin) Emissions() []string { return []string{"io.output"} }

// handleBeforeLLMRequest checks every ceiling against the request's tags.
// Block ceilings veto with a reason; downgrade-model ceilings rewrite
// req.Model to the cheapest available candidate.
func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	for _, c := range p.ceilings {
		bucketKey, ok := bucketKeyFor(c, req)
		if !ok {
			continue
		}
		totals := p.snapshotTotals(c, bucketKey)
		exceeded, reason := c.exceeded(totals)
		if !exceeded {
			c.maybeWarn(p, bucketKey, totals)
			continue
		}
		switch c.OnExceed {
		case actionWarn:
			p.emitOutput(c.message(reason))
		case actionDowngrade:
			p.applyDowngrade(req, c, reason)
		default: // block
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: reason}
			p.emitOutput(c.message(reason))
			return
		}
	}
}

// handleLLMResponse adds the response's usage to every bucket the request
// belongs to. We re-derive bucket keys from req.Tags via the response's
// Tags (provider copies them onto the response).
func (p *Plugin) handleLLMResponse(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	for _, c := range p.ceilings {
		bucketKey, ok := bucketKeyForResponse(c, resp)
		if !ok {
			continue
		}
		p.addUsage(c, bucketKey, resp)
	}
}

// snapshotTotals returns the current totals for (dim, key). Loads from
// storage when the dim is tenant; reads in-memory accumulator otherwise.
// Day windows that have rolled over reset to zero.
func (p *Plugin) snapshotTotals(c ceiling, key string) usageTotals {
	if c.Dimension == dimTenant && p.store != nil {
		return p.loadTenantTotals(key, c.Window)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.totals[c.cacheKey(key)]
	if !ok {
		return usageTotals{WindowStart: time.Now()}
	}
	if c.Window == windowDay && hasRolledOver(t.WindowStart) {
		return usageTotals{WindowStart: time.Now()}
	}
	return *t
}

// addUsage increments the bucket counters. Persists when the dimension is
// tenant; otherwise writes in-memory.
func (p *Plugin) addUsage(c ceiling, key string, resp events.LLMResponse) {
	if c.Dimension == dimTenant && p.store != nil {
		p.persistTenantUsage(key, c.Window, resp)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ck := c.cacheKey(key)
	t, ok := p.totals[ck]
	if !ok || (c.Window == windowDay && hasRolledOver(t.WindowStart)) {
		t = &usageTotals{WindowStart: time.Now()}
		p.totals[ck] = t
	}
	t.InputTokens += resp.Usage.PromptTokens
	t.OutputTokens += resp.Usage.CompletionTokens
	t.TotalTokens += resp.Usage.TotalTokens
	t.CostUSD += resp.CostUSD
}

// applyDowngrade picks the cheapest candidate from downgrade_candidates
// using the embedded pricing table, and rewrites req.Model. If no
// candidate can be found (empty config or none in the price table), the
// request is left unchanged — better to ship at full price than 500.
func (p *Plugin) applyDowngrade(req *events.LLMRequest, c ceiling, reason string) {
	if len(p.downgradeCandidates) == 0 {
		p.logger.Warn("downgrade-model triggered but downgrade_candidates is empty",
			"dimension", c.Dimension, "reason", reason)
		return
	}
	// Filter out the model we're already on so we strictly downgrade.
	cands := make([]string, 0, len(p.downgradeCandidates))
	for _, m := range p.downgradeCandidates {
		if m != req.Model {
			cands = append(cands, m)
		}
	}
	cheapest := p.pricing.CheapestModel(cands)
	if cheapest == "" {
		p.logger.Warn("downgrade-model could not find a cheaper candidate",
			"current", req.Model, "candidates", strings.Join(p.downgradeCandidates, ","))
		return
	}
	prev := req.Model
	req.Model = cheapest
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["_downgraded_by"] = pluginID
	req.Metadata["_downgraded_from"] = prev
	req.Metadata["_downgrade_reason"] = reason
	p.logger.Info("downgraded model after budget exceeded",
		"from", prev, "to", cheapest, "reason", reason)
}

func (p *Plugin) emitOutput(msg string) {
	_ = p.bus.Emit("io.output", events.AgentOutput{Content: msg, Role: "system"})
}

// bucketKeyFor returns the key the ceiling applies to for the given
// request. Returns false when the dimension's tag is absent (tenant /
// source_plugin without a value mean the ceiling doesn't apply).
func bucketKeyFor(c ceiling, req *events.LLMRequest) (string, bool) {
	switch c.Dimension {
	case dimSession:
		return "_session", true
	case dimTenant:
		v := req.Tags["tenant"]
		if v == "" || (c.Match != "" && v != c.Match) {
			return "", false
		}
		return v, true
	case dimSourcePlugin:
		v := req.Tags["source_plugin"]
		if v == "" {
			v, _ = req.Metadata["_source"].(string)
		}
		if v == "" || (c.Match != "" && v != c.Match) {
			return "", false
		}
		return v, true
	}
	return "", false
}

// bucketKeyForResponse mirrors bucketKeyFor against an llm.response.
// Providers propagate request Tags onto responses, so the same lookup
// works.
func bucketKeyForResponse(c ceiling, resp events.LLMResponse) (string, bool) {
	switch c.Dimension {
	case dimSession:
		return "_session", true
	case dimTenant:
		v := resp.Tags["tenant"]
		if v == "" || (c.Match != "" && v != c.Match) {
			return "", false
		}
		return v, true
	case dimSourcePlugin:
		v := resp.Tags["source_plugin"]
		if v == "" {
			if resp.Metadata != nil {
				v, _ = resp.Metadata["_source"].(string)
			}
		}
		if v == "" || (c.Match != "" && v != c.Match) {
			return "", false
		}
		return v, true
	}
	return "", false
}

func (c ceiling) cacheKey(bucketKey string) string {
	return c.Dimension + "|" + c.Window + "|" + bucketKey
}

// exceeded checks every configured limit and returns the first that's hit.
func (c ceiling) exceeded(t usageTotals) (bool, string) {
	if c.MaxInput > 0 && t.InputTokens >= c.MaxInput {
		return true, fmt.Sprintf("%s budget exceeded (input %d/%d tokens)", c.Dimension, t.InputTokens, c.MaxInput)
	}
	if c.MaxOutput > 0 && t.OutputTokens >= c.MaxOutput {
		return true, fmt.Sprintf("%s budget exceeded (output %d/%d tokens)", c.Dimension, t.OutputTokens, c.MaxOutput)
	}
	if c.MaxTotal > 0 && t.TotalTokens >= c.MaxTotal {
		return true, fmt.Sprintf("%s budget exceeded (%d/%d tokens)", c.Dimension, t.TotalTokens, c.MaxTotal)
	}
	if c.MaxUSD > 0 && t.CostUSD >= c.MaxUSD {
		return true, fmt.Sprintf("%s budget exceeded ($%.4f / $%.4f)", c.Dimension, t.CostUSD, c.MaxUSD)
	}
	return false, ""
}

// maybeWarn emits a one-shot warning when a bucket crosses 80% utilisation.
// Stored on the in-memory accumulator so we don't spam every request.
func (c ceiling) maybeWarn(p *Plugin, bucketKey string, t usageTotals) {
	const warnAt = 0.80
	util := c.utilisation(t)
	if util < warnAt {
		return
	}
	p.mu.Lock()
	emitted := p.warningEmitted[c.cacheKey(bucketKey)]
	if !emitted {
		p.warningEmitted[c.cacheKey(bucketKey)] = true
	}
	p.mu.Unlock()
	if emitted {
		return
	}
	_ = p.bus.Emit("io.output", events.AgentOutput{
		Content: fmt.Sprintf("Warning: %s budget at %.0f%%", c.Dimension, util*100),
		Role:    "system",
	})
}

// utilisation returns the maximum 0..1 ratio across all configured limits.
func (c ceiling) utilisation(t usageTotals) float64 {
	var max float64
	if c.MaxInput > 0 {
		max = ratio(max, float64(t.InputTokens)/float64(c.MaxInput))
	}
	if c.MaxOutput > 0 {
		max = ratio(max, float64(t.OutputTokens)/float64(c.MaxOutput))
	}
	if c.MaxTotal > 0 {
		max = ratio(max, float64(t.TotalTokens)/float64(c.MaxTotal))
	}
	if c.MaxUSD > 0 {
		max = ratio(max, t.CostUSD/c.MaxUSD)
	}
	return max
}

func (c ceiling) message(reason string) string {
	if c.Message != "" {
		return c.Message
	}
	return reason
}

func ratio(curr, candidate float64) float64 {
	if candidate > curr {
		return candidate
	}
	return curr
}

func hasRolledOver(start time.Time) bool {
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return start.UTC().Before(dayStart)
}

func anyTenantCeiling(cs []ceiling) bool {
	for _, c := range cs {
		if c.Dimension == dimTenant {
			return true
		}
	}
	return false
}

// mergedPricing assembles a price table covering every model the gate
// might be asked to compare. Defaults from all three providers are
// concatenated so downgrade-model can pick across vendor boundaries.
func mergedPricing(cfg map[string]any) *pricing.Table {
	tbl := pricing.NewTable("multi")
	for _, prov := range []string{pricing.ProviderAnthropic, pricing.ProviderOpenAI, pricing.ProviderGemini} {
		src := pricing.DefaultsFor(prov)
		for _, m := range src.Models() {
			r, _ := src.Get(m)
			tbl.Set(m, r)
		}
	}
	if raw, ok := cfg["pricing"].(map[string]any); ok {
		tbl.Merge(raw)
	}
	return tbl
}

// --- Tenant persistence -----------------------------------------------------
//
// One row per (tenant, window_start_day). We keep the schema flat: row
// counters accumulate every llm.response, day windows roll by writing a
// new row. Reads pick the row matching today's UTC date.

const tenantSchema = `
CREATE TABLE IF NOT EXISTS token_budget_tenant (
	tenant       TEXT NOT NULL,
	window_start TEXT NOT NULL,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens  INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0,
	updated_at    TEXT NOT NULL,
	PRIMARY KEY (tenant, window_start)
)`

func initTenantSchema(db *sql.DB) error {
	_, err := db.Exec(tenantSchema)
	return err
}

// windowStartFor returns the canonical window_start string for a given
// window type. Day windows round to UTC midnight; session windows use a
// fixed sentinel so the row is shared across sessions.
func windowStartFor(window string) string {
	if window == windowDay {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	return "all-time"
}

func (p *Plugin) loadTenantTotals(tenant, window string) usageTotals {
	row := p.store.DB().QueryRow(`
		SELECT input_tokens, output_tokens, total_tokens, cost_usd, window_start
		FROM token_budget_tenant
		WHERE tenant = ? AND window_start = ?`,
		tenant, windowStartFor(window))

	var t usageTotals
	var startStr string
	if err := row.Scan(&t.InputTokens, &t.OutputTokens, &t.TotalTokens, &t.CostUSD, &startStr); err != nil {
		// no row yet → zeroed totals
		return usageTotals{WindowStart: time.Now()}
	}
	if ts, err := time.Parse(time.RFC3339, startStr); err == nil {
		t.WindowStart = ts
	} else {
		t.WindowStart = time.Now()
	}
	return t
}

func (p *Plugin) persistTenantUsage(tenant, window string, resp events.LLMResponse) {
	now := time.Now().UTC().Format(time.RFC3339)
	wstart := windowStartFor(window)
	_, err := p.store.DB().Exec(`
		INSERT INTO token_budget_tenant
			(tenant, window_start, input_tokens, output_tokens, total_tokens, cost_usd, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, window_start) DO UPDATE SET
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens = total_tokens + excluded.total_tokens,
			cost_usd = cost_usd + excluded.cost_usd,
			updated_at = excluded.updated_at`,
		tenant, wstart,
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens, resp.CostUSD,
		now,
	)
	if err != nil {
		p.logger.Error("token_budget: persist tenant usage", "tenant", tenant, "error", err)
	}
}

// --- Config parsing ---------------------------------------------------------

func parseCeilings(cfg map[string]any) ([]ceiling, error) {
	var out []ceiling

	// Backward compat: legacy max_tokens key becomes a session ceiling.
	if v, ok := numericInt(cfg["max_tokens"]); ok && v > 0 {
		out = append(out, ceiling{
			Dimension: dimSession,
			Window:    windowSession,
			OnExceed:  actionBlock,
			MaxTotal:  v,
			Message:   stringOr(cfg["message"], "Token budget exhausted for this session."),
		})
	}

	defaultOnExceed := actionBlock
	if v, ok := cfg["on_exceed"].(string); ok && v != "" {
		defaultOnExceed = v
	}

	rawCeilings, _ := cfg["ceilings"].([]any)
	for i, raw := range rawCeilings {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ceilings[%d]: must be a mapping", i)
		}
		c, err := parseCeiling(entry, defaultOnExceed)
		if err != nil {
			return nil, fmt.Errorf("ceilings[%d]: %w", i, err)
		}
		out = append(out, c)
	}
	return out, nil
}

func parseCeiling(m map[string]any, defaultOnExceed string) (ceiling, error) {
	c := ceiling{
		Dimension: stringOr(m["dimension"], dimSession),
		Window:    stringOr(m["window"], windowSession),
		Match:     stringOr(m["match"], ""),
		OnExceed:  stringOr(m["on_exceed"], defaultOnExceed),
		Message:   stringOr(m["message"], ""),
	}
	switch c.Dimension {
	case dimSession, dimTenant, dimSourcePlugin:
	default:
		return c, fmt.Errorf("unsupported dimension %q", c.Dimension)
	}
	switch c.OnExceed {
	case actionBlock, actionWarn, actionDowngrade:
	default:
		return c, fmt.Errorf("unsupported on_exceed %q", c.OnExceed)
	}
	switch c.Window {
	case windowSession, windowDay:
	default:
		return c, fmt.Errorf("unsupported window %q", c.Window)
	}
	if v, ok := numericInt(m["max_input_tokens"]); ok {
		c.MaxInput = v
	}
	if v, ok := numericInt(m["max_output_tokens"]); ok {
		c.MaxOutput = v
	}
	if v, ok := numericInt(m["max_total_tokens"]); ok {
		c.MaxTotal = v
	}
	if v, ok := numericFloat(m["max_usd"]); ok {
		c.MaxUSD = v
	}
	if v, ok := numericFloat(m["max_usd_per_session"]); ok && c.Window == windowSession {
		c.MaxUSD = v
	}
	if v, ok := numericFloat(m["max_usd_per_day"]); ok {
		c.MaxUSD = v
		c.Window = windowDay
	}
	if c.MaxInput == 0 && c.MaxOutput == 0 && c.MaxTotal == 0 && c.MaxUSD == 0 {
		return c, fmt.Errorf("must set at least one of max_input_tokens, max_output_tokens, max_total_tokens, max_usd, max_usd_per_session, max_usd_per_day")
	}
	return c, nil
}

func numericInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n != 0
	case int64:
		return int(n), n != 0
	case float64:
		return int(n), n != 0
	}
	return 0, false
}

func numericFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, n != 0
	case int:
		return float64(n), n != 0
	case int64:
		return float64(n), n != 0
	}
	return 0, false
}

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}
