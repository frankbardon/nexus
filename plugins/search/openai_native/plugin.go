// Package openainative implements the search.provider capability backed by
// OpenAI's hosted web_search tool, invoked through the Responses API. Like
// its Anthropic sibling it does not go through the LLM provider plugin; it
// issues a direct HTTP call to /v1/responses with the built-in tool so the
// adapter remains usable even when the project's LLM is not OpenAI.
package openainative

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
	pluginID    = "nexus.search.openai_native"
	pluginName  = "OpenAI Native Search Provider"
	version     = "0.1.0"
	responseURL = "https://api.openai.com/v1/responses"
)

// Plugin answers search.request via a one-shot OpenAI Responses call with
// the built-in web_search tool.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey  string
	baseURL string
	model   string
	client  *http.Client
	unsubs  []func()
	timeout time.Duration
}

func New() engine.Plugin {
	return &Plugin{
		baseURL: responseURL,
		model:   "gpt-4o-mini",
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
		Description: "Web search via OpenAI's server-side web_search tool (Responses API).",
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
			envVar = "OPENAI_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("openai_native: no API key configured (set api_key in config or OPENAI_API_KEY env var)")
	}

	if m, ok := ctx.Config["model"].(string); ok && m != "" {
		p.model = m
	}
	if u, ok := ctx.Config["base_url"].(string); ok && u != "" {
		p.baseURL = u
	}
	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("openai_native: invalid timeout %q: %w", ts, err)
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
		"model":       p.model,
		"input":       buildSearchPrompt(req.Query, req.Freshness, count),
		"tools":       []any{map[string]any{"type": "web_search"}},
		"tool_choice": map[string]any{"type": "web_search"},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai returned HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var decoded openAIResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return extractResults(decoded, count), nil
}

func buildSearchPrompt(query, freshness string, count int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Use the web_search tool (only once) to find up to %d recent results for the query below. ", count)
	b.WriteString("Do not answer from memory. Cite every source you use.\n\n")
	fmt.Fprintf(&b, "Query: %s\n", query)
	if f := strings.TrimSpace(freshness); f != "" {
		fmt.Fprintf(&b, "Freshness: prefer results from the last %s.\n", f)
	}
	return b.String()
}

// openAIResponse captures the Responses API shape we care about — message
// content with URL citations and separate web_search_call blocks.
type openAIResponse struct {
	Output []struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Annotations []struct {
				Type       string `json:"type"`
				URL        string `json:"url"`
				Title      string `json:"title"`
				StartIndex int    `json:"start_index"`
				EndIndex   int    `json:"end_index"`
			} `json:"annotations"`
		} `json:"content"`
	} `json:"output"`
}

func extractResults(resp openAIResponse, limit int) []events.SearchResult {
	seen := map[string]struct{}{}
	var results []events.SearchResult

	for _, block := range resp.Output {
		if block.Type != "message" {
			continue
		}
		for _, c := range block.Content {
			for _, a := range c.Annotations {
				if a.Type != "url_citation" || a.URL == "" {
					continue
				}
				if _, dup := seen[a.URL]; dup {
					continue
				}
				seen[a.URL] = struct{}{}
				snippet := ""
				if a.StartIndex < a.EndIndex && a.EndIndex <= len(c.Text) {
					snippet = strings.TrimSpace(c.Text[a.StartIndex:a.EndIndex])
				}
				results = append(results, events.SearchResult{
					Title:   a.Title,
					URL:     a.URL,
					Snippet: snippet,
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
