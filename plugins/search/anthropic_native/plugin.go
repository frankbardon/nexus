// Package anthropicnative implements the search.provider capability backed
// by Anthropic's server-side web_search tool. Unlike the Brave adapter this
// does not call a dedicated search API — it makes a minimal Messages call
// with the web_search_20250305 tool and harvests the citations Claude
// attaches to its response.
//
// Shipped as a standalone plugin rather than a feature of the Anthropic LLM
// provider so a project can mix and match (e.g. use OpenAI for its LLM but
// still pay Anthropic a search query for provider-grade results). The scope
// decision is documented in docs/src/guides/web-search.md.
package anthropicnative

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
	pluginID   = "nexus.search.anthropic_native"
	pluginName = "Anthropic Native Search Provider"
	version    = "0.1.0"
	apiURL     = "https://api.anthropic.com/v1/messages"
	toolType   = "web_search_20250305"
)

// Plugin answers search.request via a one-shot Anthropic Messages call with
// the built-in web_search tool enabled.
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
		model:   "claude-haiku-4-5-20251001",
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
		Description: "Web search via Anthropic's server-side web_search tool.",
	}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	if key, ok := ctx.Config["api_key"].(string); ok && key != "" {
		p.apiKey = key
	} else {
		envVar, _ := ctx.Config["api_key_env"].(string)
		if envVar == "" {
			envVar = "ANTHROPIC_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("anthropic_native: no API key configured (set api_key in config or ANTHROPIC_API_KEY env var)")
	}

	if m, ok := ctx.Config["model"].(string); ok && m != "" {
		p.model = m
	}
	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("anthropic_native: invalid timeout %q: %w", ts, err)
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

	tool := map[string]any{
		"type":     toolType,
		"name":     "web_search",
		"max_uses": count,
	}

	body := map[string]any{
		"model":      p.model,
		"max_tokens": 4096,
		"tools":      []any{tool},
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": buildSearchPrompt(req.Query, req.Freshness),
			},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic returned HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var decoded anthropicResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return extractResults(decoded, count), nil
}

// buildSearchPrompt nudges Claude to actually invoke web_search rather than
// reply from parametric knowledge. Anthropic's tool loop handles the heavy
// lifting; we just need it to commit to one search call.
func buildSearchPrompt(query, freshness string) string {
	var b strings.Builder
	b.WriteString("Use the web_search tool to find current information for the following query. ")
	b.WriteString("Do not answer from memory. Return only the search results.\n\n")
	fmt.Fprintf(&b, "Query: %s\n", query)
	if f := strings.TrimSpace(freshness); f != "" {
		fmt.Fprintf(&b, "Freshness: prefer results from the last %s.\n", f)
	}
	return b.String()
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`

		// server_tool_use / web_search_tool_result blocks carry the actual hits.
		ID      string          `json:"id,omitempty"`
		Name    string          `json:"name,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`

		// Citations on text blocks carry URL + title when Claude cites sources.
		Citations []struct {
			Type           string `json:"type"`
			URL            string `json:"url"`
			Title          string `json:"title"`
			CitedText      string `json:"cited_text"`
			EncryptedIndex string `json:"encrypted_index"`
		} `json:"citations,omitempty"`
	} `json:"content"`
}

// extractResults harvests results from either the web_search_tool_result
// block (preferred, structured) or the citations attached to text blocks
// (fallback, still useful).
func extractResults(resp anthropicResponse, limit int) []events.SearchResult {
	seen := map[string]struct{}{}
	var results []events.SearchResult

	for _, block := range resp.Content {
		if block.Type == "web_search_tool_result" {
			var items []struct {
				Type             string `json:"type"`
				URL              string `json:"url"`
				Title            string `json:"title"`
				EncryptedContent string `json:"encrypted_content"`
				PageAge          string `json:"page_age"`
			}
			if err := json.Unmarshal(block.Content, &items); err == nil {
				for _, it := range items {
					if it.URL == "" {
						continue
					}
					if _, dup := seen[it.URL]; dup {
						continue
					}
					seen[it.URL] = struct{}{}
					r := events.SearchResult{
						Title: it.Title,
						URL:   it.URL,
					}
					if it.PageAge != "" {
						if t, err := time.Parse(time.RFC3339, it.PageAge); err == nil {
							r.PublishedAt = t
						}
					}
					results = append(results, r)
				}
			}
		}
	}

	if len(results) == 0 {
		for _, block := range resp.Content {
			for _, c := range block.Citations {
				if c.URL == "" {
					continue
				}
				if _, dup := seen[c.URL]; dup {
					continue
				}
				seen[c.URL] = struct{}{}
				results = append(results, events.SearchResult{
					Title:   c.Title,
					URL:     c.URL,
					Snippet: c.CitedText,
				})
			}
		}
	}

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
