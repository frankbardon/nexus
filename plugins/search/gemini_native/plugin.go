// Package geminative implements the search.provider capability backed by
// Gemini's server-side google_search tool. Mirrors the anthropic_native and
// openai_native search plugins so projects can pick a search provider
// independently of their primary LLM provider.
package geminative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.search.gemini_native"
	pluginName = "Gemini Native Search Provider"
	version    = "0.1.0"
)

// Plugin answers search.request via a one-shot generateContent call with the
// google_search tool enabled.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey  string
	model   string
	client  *http.Client
	unsubs  []func()
	timeout time.Duration
}

func New() engine.Plugin {
	return &Plugin{
		model:   "gemini-2.5-flash",
		timeout: 30 * time.Second,
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "search.provider",
		Description: "Web search via Gemini's server-side google_search tool.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar != "" {
			p.apiKey = os.Getenv(envVar)
		} else {
			if v := os.Getenv("GEMINI_API_KEY"); v != "" {
				p.apiKey = v
			} else if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
				p.apiKey = v
			}
		}
	}
	if p.apiKey == "" {
		return fmt.Errorf("gemini_native: no API key configured (set api_key in config or GEMINI_API_KEY / GOOGLE_API_KEY env var)")
	}

	if m, ok := ctx.Config["model"].(string); ok && m != "" {
		p.model = m
	}
	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("gemini_native: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	p.client = &http.Client{Timeout: p.timeout}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("search.request", p.handleSearch,
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
	return []engine.EventSubscription{
		{EventType: "search.request", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string { return nil }

func (p *Plugin) handleSearch(event engine.Event[any]) {
	req, ok := event.Payload.(*events.SearchRequest)
	if !ok {
		return
	}
	if req.Provider != "" {
		return
	}

	results, err := p.search(req)
	req.Provider = pluginID
	if err != nil {
		req.Error = err.Error()
		return
	}
	req.Results = results
}

func (p *Plugin) search(req *events.SearchRequest) ([]events.SearchResult, error) {
	count := req.Count
	if count <= 0 {
		count = 10
	}

	body := map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": buildSearchPrompt(req.Query, req.Freshness)}},
		}},
		"tools": []map[string]any{{"google_search": map[string]any{}}},
		"generationConfig": map[string]any{
			"maxOutputTokens": 4096,
			"temperature":     0.0,
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", p.model)

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("x-goog-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini returned HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var decoded geminiResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return extractResults(decoded, count), nil
}

func buildSearchPrompt(query, freshness string) string {
	var b strings.Builder
	b.WriteString("Use google_search to look up current information for the query below. ")
	b.WriteString("Do not answer from memory. Return concise summaries with sources.\n\n")
	fmt.Fprintf(&b, "Query: %s\n", query)
	if f := strings.TrimSpace(freshness); f != "" {
		fmt.Fprintf(&b, "Freshness: prefer results from the last %s.\n", f)
	}
	return b.String()
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		GroundingMetadata *struct {
			GroundingChunks []struct {
				Web *struct {
					URI   string `json:"uri"`
					Title string `json:"title"`
				} `json:"web,omitempty"`
			} `json:"groundingChunks"`
			GroundingSupports []struct {
				Segment struct {
					Text string `json:"text"`
				} `json:"segment"`
				GroundingChunkIndices []int `json:"groundingChunkIndices"`
			} `json:"groundingSupports"`
		} `json:"groundingMetadata,omitempty"`
	} `json:"candidates"`
}

func extractResults(resp geminiResponse, limit int) []events.SearchResult {
	if len(resp.Candidates) == 0 || resp.Candidates[0].GroundingMetadata == nil {
		return nil
	}

	gm := resp.Candidates[0].GroundingMetadata
	seen := make(map[string]struct{}, len(gm.GroundingChunks))

	// Build a chunk-index → snippet map by walking groundingSupports.
	snippetsByChunk := make(map[int][]string)
	for _, s := range gm.GroundingSupports {
		text := strings.TrimSpace(s.Segment.Text)
		if text == "" {
			continue
		}
		for _, idx := range s.GroundingChunkIndices {
			snippetsByChunk[idx] = append(snippetsByChunk[idx], text)
		}
	}

	var out []events.SearchResult
	for i, chunk := range gm.GroundingChunks {
		if chunk.Web == nil || chunk.Web.URI == "" {
			continue
		}
		if _, dup := seen[chunk.Web.URI]; dup {
			continue
		}
		seen[chunk.Web.URI] = struct{}{}

		snippet := ""
		if snips, ok := snippetsByChunk[i]; ok && len(snips) > 0 {
			snippet = snips[0]
		}

		out = append(out, events.SearchResult{
			Title:   chunk.Web.Title,
			URL:     chunk.Web.URI,
			Snippet: snippet,
		})
	}

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
