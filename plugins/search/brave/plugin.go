// Package brave implements a search.provider capability backed by the Brave
// Search API (https://api.search.brave.com/). Requires an API key obtained
// from the Brave Search dashboard.
package brave

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.search.brave"
	pluginName = "Brave Search Provider"
	version    = "0.1.0"
	apiURL     = "https://api.search.brave.com/res/v1/web/search"
)

// Plugin advertises the search.provider capability and answers
// search.request events by calling the Brave Search REST API.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	apiKey  string
	client  *http.Client
	unsubs  []func()
	timeout time.Duration
}

func New() engine.Plugin {
	return &Plugin{timeout: 15 * time.Second}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{
		Name:        "search.provider",
		Description: "Web search via the Brave Search API.",
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
			envVar = "BRAVE_API_KEY"
		}
		p.apiKey = os.Getenv(envVar)
	}
	if p.apiKey == "" {
		return fmt.Errorf("brave: no API key configured (set api_key in config or BRAVE_API_KEY env var)")
	}

	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("brave: invalid timeout %q: %w", ts, err)
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
	// Another provider already answered (multi-provider race, pinned
	// provider, etc.) — don't overwrite their results.
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
	if count > 20 {
		count = 20 // Brave API hard cap on web results per page.
	}

	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("count", strconv.Itoa(count))
	if req.SafeSearch != "" {
		q.Set("safesearch", normalizeSafe(req.SafeSearch))
	}
	if req.Language != "" {
		q.Set("search_lang", strings.ToLower(req.Language))
	}
	if f := normalizeFreshness(req.Freshness); f != "" {
		q.Set("freshness", f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "GET", apiURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("brave returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var parsed braveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	results := make([]events.SearchResult, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		hit := events.SearchResult{
			Title:   stripTags(r.Title),
			URL:     r.URL,
			Snippet: stripTags(r.Description),
			Source:  r.NetLoc,
		}
		if r.Age != "" {
			if t, err := time.Parse(time.RFC3339, r.Age); err == nil {
				hit.PublishedAt = t
			}
		}
		results = append(results, hit)
	}
	return results, nil
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			NetLoc      string `json:"netloc"`
			Age         string `json:"age"` // RFC3339 when present
		} `json:"results"`
	} `json:"web"`
}

func normalizeSafe(s string) string {
	switch strings.ToLower(s) {
	case "off", "none":
		return "off"
	case "strict":
		return "strict"
	default:
		return "moderate"
	}
}

func normalizeFreshness(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "day", "pd":
		return "pd"
	case "week", "pw":
		return "pw"
	case "month", "pm":
		return "pm"
	case "year", "py":
		return "py"
	}
	return ""
}

// stripTags removes Brave's <strong> highlight markup from titles and snippets.
func stripTags(s string) string {
	s = strings.ReplaceAll(s, "<strong>", "")
	s = strings.ReplaceAll(s, "</strong>", "")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
