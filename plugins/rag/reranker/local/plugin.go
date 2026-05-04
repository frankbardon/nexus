// Package local implements the search.reranker capability with a pure-Go
// TF-IDF cosine reranker. No external API calls, no model files, no extra
// dependencies — useful for offline / cost-sensitive deployments and as the
// default when no Cohere or Jina key is configured.
//
// Quality is materially below a real cross-encoder (BGE Reranker, Cohere
// Rerank). The intent here is to provide a working reranker shape so the
// hybrid orchestrator's reranker hook is exercised end-to-end. A future
// phase can swap the scorer for an ONNX-backed BGE Reranker behind a build
// tag without changing the plugin's external surface.
package local

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.rag.reranker.local"
	pluginName = "Local TF-IDF Reranker"
	version    = "0.1.0"
)

// Plugin advertises search.reranker via a pure-Go scorer.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	minTokenLen int
	stopwords   map[string]bool

	unsubs []func()
}

func New() engine.Plugin {
	return &Plugin{
		minTokenLen: 2,
		stopwords:   defaultStopwords(),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "search.reranker",
		Description: "Local pure-Go TF-IDF cosine reranker (offline, no API calls).",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["min_token_length"].(int); ok && v > 0 {
		p.minTokenLen = v
	}
	if v, ok := ctx.Config["disable_stopwords"].(bool); ok && v {
		p.stopwords = nil
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("reranker.rerank", p.handleRerank,
			engine.WithPriority(50), engine.WithSource(pluginID)),
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
	return []engine.EventSubscription{{EventType: "reranker.rerank", Priority: 50}}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handleRerank(event engine.Event[any]) {
	req, ok := event.Payload.(*events.RerankRequest)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}
	req.Provider = pluginID

	if req.Query == "" || len(req.Docs) == 0 {
		return
	}
	topN := req.TopN
	if topN <= 0 || topN > len(req.Docs) {
		topN = len(req.Docs)
	}

	queryTokens := p.tokenize(req.Query)
	docTokens := make([][]string, len(req.Docs))
	for i, d := range req.Docs {
		docTokens[i] = p.tokenize(d.Content)
	}

	df := documentFrequency(docTokens)
	queryVec := tfidfVector(queryTokens, df, len(req.Docs))
	scored := make([]events.RerankResult, 0, len(req.Docs))
	for i, toks := range docTokens {
		docVec := tfidfVector(toks, df, len(req.Docs))
		score := cosine(queryVec, docVec)
		scored = append(scored, events.RerankResult{
			ID:    req.Docs[i].ID,
			Index: i,
			Score: float32(score),
		})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > topN {
		scored = scored[:topN]
	}
	req.Results = scored
}

// tokenize lowercases input and splits on non-letter / non-digit boundaries.
// Drops tokens shorter than minTokenLen and any in the stopword set.
func (p *Plugin) tokenize(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ToLower(s)
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		if len(tok) < p.minTokenLen {
			return
		}
		if p.stopwords != nil && p.stopwords[tok] {
			return
		}
		out = append(out, tok)
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// documentFrequency returns the per-term document-frequency map across the
// candidate corpus. Terms appearing in many docs receive lower IDF weight.
func documentFrequency(docTokens [][]string) map[string]int {
	df := make(map[string]int)
	for _, toks := range docTokens {
		seen := make(map[string]bool, len(toks))
		for _, t := range toks {
			if seen[t] {
				continue
			}
			seen[t] = true
			df[t]++
		}
	}
	return df
}

// tfidfVector builds a sparse term-weight map for one token list. IDF uses
// the standard smoothed log((N + 1) / (df + 1)) + 1 form so unseen-in-corpus
// terms (i.e. query-only terms) get a positive weight rather than collapsing
// to zero.
func tfidfVector(tokens []string, df map[string]int, corpusSize int) map[string]float64 {
	if len(tokens) == 0 {
		return nil
	}
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	out := make(map[string]float64, len(tf))
	for t, c := range tf {
		idf := math.Log(float64(corpusSize+1)/float64(df[t]+1)) + 1
		out[t] = float64(c) * idf
	}
	return out
}

func cosine(a, b map[string]float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	for k, va := range a {
		na += va * va
		if vb, ok := b[k]; ok {
			dot += va * vb
		}
	}
	for _, vb := range b {
		nb += vb * vb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// defaultStopwords returns a small English stopword set. Conservative — we
// drop only the most common function words so identifiers and rare terms
// keep their weight.
func defaultStopwords() map[string]bool {
	return map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true,
		"is": true, "are": true, "was": true, "were": true, "be": true,
		"of": true, "in": true, "on": true, "at": true, "to": true,
		"for": true, "with": true, "as": true, "by": true, "from": true,
		"that": true, "this": true, "it": true, "its": true,
	}
}
