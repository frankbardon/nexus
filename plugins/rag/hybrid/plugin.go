// Package hybrid implements the search.hybrid capability — a fusion
// orchestrator that runs vector and lexical retrieval in parallel and
// combines the rankings. Two fusion strategies ship: RRF (default,
// weight-free) and weighted (linear combination over min-max-normalized
// scores). Per-query LexicalBias tilts either strategy without rewriting
// config.
//
// Consumers (tools/knowledge_search, memory/vector) emit hybrid.query when
// the search.hybrid capability is active and fall back to direct vector
// query otherwise.
package hybrid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.rag.hybrid"
	pluginName = "Hybrid Retrieval Orchestrator"
	version    = "0.1.0"

	defaultRetrieveK = 50
	defaultFuseTo    = 20
	defaultRRFK      = 60.0
)

// Plugin orchestrates parallel vector + lexical retrieval and fuses results.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	fusion         string  // "rrf" | "weighted"
	rrfK           float64 // RRF smoothing constant
	weightVector   float64
	weightLexical  float64
	retrieveK      int // per-backend candidate count
	fuseTo         int // post-fusion top-N
	embeddingModel string
	// reranker enables a post-fusion reranker pass via the search.reranker
	// capability. Off by default so single-mode + hybrid-only deployments
	// keep their existing latency profile. When on and no provider is
	// active, the orchestrator logs once at boot and proceeds without
	// reranking — never fails the query.
	rerankerEnabled bool
	hasReranker     bool

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{
		fusion:        "rrf",
		rrfK:          defaultRRFK,
		weightVector:  0.7,
		weightLexical: 0.3,
		retrieveK:     defaultRetrieveK,
		fuseTo:        defaultFuseTo,
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{
		{Capability: "embeddings.provider"},
		{Capability: "vector.store"},
		{Capability: "search.lexical"},
	}
}

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "search.hybrid",
		Description: "Fused retrieval over vector + lexical stores via RRF or weighted combination.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["fusion"].(string); ok && v != "" {
		switch strings.ToLower(v) {
		case "rrf", "weighted":
			p.fusion = strings.ToLower(v)
		default:
			return fmt.Errorf("rag/hybrid: unknown fusion %q (want rrf|weighted)", v)
		}
	}
	if v, ok := ctx.Config["rrf_k"].(int); ok && v > 0 {
		p.rrfK = float64(v)
	}
	if v, ok := ctx.Config["rrf_k"].(float64); ok && v > 0 {
		p.rrfK = v
	}
	if w, ok := ctx.Config["weights"].(map[string]any); ok {
		if v, ok := numConfig(w["vector"]); ok {
			p.weightVector = v
		}
		if v, ok := numConfig(w["lexical"]); ok {
			p.weightLexical = v
		}
	}
	if v, ok := ctx.Config["retrieve_k"].(int); ok && v > 0 {
		p.retrieveK = v
	}
	if v, ok := ctx.Config["retrieve_k"].(float64); ok && v > 0 {
		p.retrieveK = int(v)
	}
	if v, ok := ctx.Config["fuse_to"].(int); ok && v > 0 {
		p.fuseTo = v
	}
	if v, ok := ctx.Config["fuse_to"].(float64); ok && v > 0 {
		p.fuseTo = int(v)
	}
	if v, ok := ctx.Config["embedding_model"].(string); ok {
		p.embeddingModel = v
	}
	if r, ok := ctx.Config["reranker"].(map[string]any); ok {
		if v, ok := r["enabled"].(bool); ok {
			p.rerankerEnabled = v
		}
	}
	p.hasReranker = len(ctx.Capabilities["search.reranker"]) > 0
	if p.rerankerEnabled && !p.hasReranker {
		p.logger.Warn("rag/hybrid: reranker.enabled=true but no search.reranker provider active; proceeding without rerank")
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("hybrid.query", p.handleQuery,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	p.logger.Info("rag/hybrid ready",
		"fusion", p.fusion,
		"retrieve_k", p.retrieveK,
		"fuse_to", p.fuseTo)
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
		{EventType: "hybrid.query", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"embeddings.request",
		"vector.query",
		"lexical.query",
		"reranker.rerank",
	}
}

func (p *Plugin) handleQuery(event engine.Event[any]) {
	req, ok := event.Payload.(*events.HybridQuery)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if req.Namespace == "" {
		req.Error = "namespace required"
		return
	}
	if req.Query == "" && len(req.Vector) == 0 {
		req.Error = "either Query or Vector required"
		return
	}

	// Embed if the caller did not pre-embed.
	vec := req.Vector
	if len(vec) == 0 {
		embReq := &events.EmbeddingsRequest{Texts: []string{req.Query}, Model: p.embeddingModel}
		_ = p.bus.Emit("embeddings.request", embReq)
		if embReq.Error != "" {
			req.Error = fmt.Sprintf("embed query: %s", embReq.Error)
			return
		}
		if len(embReq.Vectors) != 1 {
			req.Error = "embed query: no vector returned"
			return
		}
		vec = embReq.Vectors[0]
	}

	// Run vector + lexical queries in parallel. Errors from either side are
	// non-fatal — fusion proceeds with whichever lists came back.
	var (
		wg   sync.WaitGroup
		vecQ = &events.VectorQuery{Namespace: req.Namespace, Vector: vec, K: p.retrieveK, Filter: req.Filter}
		lexQ = &events.LexicalQuery{Namespace: req.Namespace, Query: req.Query, K: p.retrieveK, Filter: req.Filter}
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = p.bus.Emit("vector.query", vecQ)
	}()
	go func() {
		defer wg.Done()
		// Lexical needs the raw query text. If only Vector was supplied (no
		// text), skip the lexical leg and let the vector results stand.
		if req.Query == "" {
			return
		}
		_ = p.bus.Emit("lexical.query", lexQ)
	}()
	wg.Wait()

	if vecQ.Error != "" {
		p.logger.Warn("rag/hybrid: vector query failed", "err", vecQ.Error)
	}
	if lexQ.Error != "" {
		p.logger.Warn("rag/hybrid: lexical query failed", "err", lexQ.Error)
	}

	lists := p.buildRankedLists(vecQ, lexQ, req.LexicalBias)
	var fused []candidate
	switch p.fusion {
	case "weighted":
		fused = fuseWeighted(lists)
	default:
		fused = fuseRRF(lists, p.rrfK)
	}

	if p.rerankerEnabled && p.hasReranker && len(fused) > 1 {
		fused = p.applyReranker(req.Query, fused)
	}

	k := req.K
	if k <= 0 {
		k = p.fuseTo
	}
	if len(fused) > k {
		fused = fused[:k]
	}

	matches := make([]events.HybridMatch, len(fused))
	for i, c := range fused {
		matches[i] = events.HybridMatch{
			ID:         c.id,
			Content:    c.content,
			Metadata:   c.metadata,
			Score:      c.score,
			Similarity: c.similarity,
			Lexical:    c.lexical,
			Sources:    c.sources,
		}
	}
	req.Matches = matches
}

// applyReranker re-scores the fused candidate pool via the active
// search.reranker provider. Reranker errors are non-fatal — the orchestrator
// returns the un-reranked list with a warning so a flaky reranker never
// blocks knowledge retrieval. Returns the candidates re-sorted with the
// reranker score replacing the fusion score (fusion score moves nowhere —
// the rerank pass is meant to dominate the final ordering).
func (p *Plugin) applyReranker(query string, fused []candidate) []candidate {
	docs := make([]events.RerankDoc, len(fused))
	for i, c := range fused {
		docs[i] = events.RerankDoc{ID: c.id, Content: c.content}
	}
	req := &events.RerankRequest{
		Query: query,
		Docs:  docs,
		TopN:  len(fused),
	}
	_ = p.bus.Emit("reranker.rerank", req)
	if req.Error != "" {
		p.logger.Warn("rag/hybrid: reranker failed; returning fusion order", "err", req.Error)
		return fused
	}
	if len(req.Results) == 0 {
		return fused
	}

	// Build new ordering by rerank score, dropping any result whose Index
	// fell outside the input slice (defensive — providers should always echo
	// valid indices).
	out := make([]candidate, 0, len(req.Results))
	for _, r := range req.Results {
		if r.Index < 0 || r.Index >= len(fused) {
			continue
		}
		c := fused[r.Index]
		c.score = r.Score
		out = append(out, c)
	}
	return out
}

// buildRankedLists turns the vector + lexical responses into normalized
// rankedList structs. Per-query LexicalBias scales the lexical list's
// contribution; a positive value tilts toward lexical, negative toward
// vector. The bias is clamped into [-0.95, 0.95] so neither side is fully
// silenced.
func (p *Plugin) buildRankedLists(vecQ *events.VectorQuery, lexQ *events.LexicalQuery, bias float32) []rankedList {
	if bias > 0.95 {
		bias = 0.95
	}
	if bias < -0.95 {
		bias = -0.95
	}

	vectorWeight := p.weightVector
	lexicalWeight := p.weightLexical
	switch p.fusion {
	case "weighted":
		vectorWeight *= 1 - float64(bias)
		lexicalWeight *= 1 + float64(bias)
	default: // rrf — weight-free; use bias to scale RRF contribution
		vectorWeight = 1 - float64(bias)
		lexicalWeight = 1 + float64(bias)
	}

	vDocs := make([]rankedDoc, 0, len(vecQ.Matches))
	for _, m := range vecQ.Matches {
		vDocs = append(vDocs, rankedDoc{
			id: m.ID, content: m.Content, metadata: m.Metadata,
			similarity: m.Similarity,
		})
	}
	lDocs := make([]rankedDoc, 0, len(lexQ.Matches))
	for _, m := range lexQ.Matches {
		lDocs = append(lDocs, rankedDoc{
			id: m.ID, content: m.Content, metadata: m.Metadata,
			lexical: m.Score,
		})
	}

	return []rankedList{
		{source: "vector", bias: vectorWeight, docs: vDocs},
		{source: "lexical", bias: lexicalWeight, docs: lDocs},
	}
}

// numConfig coerces YAML int/float into float64 with an ok signal.
func numConfig(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
