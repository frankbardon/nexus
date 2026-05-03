// Package cohere implements the search.reranker capability backed by the
// Cohere Rerank v2 API. Direct HTTP, no SDK — keeps deps minimal.
package cohere

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID     = "nexus.rag.reranker.cohere"
	pluginName   = "Cohere Reranker"
	version      = "0.1.0"
	apiURL       = "https://api.cohere.com/v2/rerank"
	defaultModel = "rerank-english-v3.0"
)

// Plugin advertises search.reranker via the Cohere Rerank API.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey  string
	apiURL  string
	model   string
	timeout time.Duration
	client  *http.Client
	unsubs  []func()
}

func New() engine.Plugin {
	return &Plugin{
		model:   defaultModel,
		apiURL:  apiURL,
		timeout: 10 * time.Second,
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
		Description: "Cross-encoder reranker via Cohere Rerank v2 API.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if v, ok := ctx.Config["api_key"].(string); ok && v != "" {
		p.apiKey = v
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar == "" {
			envVar = "COHERE_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("cohere reranker: no API key configured (set api_key or COHERE_API_KEY env var)")
	}
	if v, ok := ctx.Config["model"].(string); ok && v != "" {
		p.model = v
	}
	if v, ok := ctx.Config["timeout_ms"].(int); ok && v > 0 {
		p.timeout = time.Duration(v) * time.Millisecond
	}
	if v, ok := ctx.Config["api_base"].(string); ok && v != "" {
		p.apiURL = v
	}
	p.client = &http.Client{Timeout: p.timeout}

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
	if p.client != nil {
		p.client.CloseIdleConnections()
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
	model := req.Model
	if model == "" {
		model = p.model
	}
	topN := req.TopN
	if topN <= 0 || topN > len(req.Docs) {
		topN = len(req.Docs)
	}

	results, err := p.rerank(req.Query, req.Docs, model, topN)
	if err != nil {
		req.Error = err.Error()
		return
	}
	req.Results = results
}

func (p *Plugin) rerank(query string, docs []events.RerankDoc, model string, topN int) ([]events.RerankResult, error) {
	contents := make([]string, len(docs))
	for i, d := range docs {
		contents[i] = d.Content
	}
	body := map[string]any{
		"model":     model,
		"query":     query,
		"documents": contents,
		"top_n":     topN,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cohere returned HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}

	var parsed cohereResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := make([]events.RerankResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		out = append(out, events.RerankResult{
			ID:    docs[r.Index].ID,
			Index: r.Index,
			Score: float32(r.RelevanceScore),
		})
	}
	return out, nil
}

type cohereResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
