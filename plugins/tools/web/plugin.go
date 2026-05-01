// Package web registers the user-facing web_search and web_fetch tools.
// The plugin itself does no searching — it emits search.request on the bus
// and relies on a capability-resolved search provider (Brave, Tavily, native
// Anthropic/OpenAI, etc.) to populate results. Fetching is done locally
// with net/http + go-readability for article extraction.
package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.web"
	pluginName = "Web Search & Fetch Tool"
	version    = "0.1.0"

	defaultUserAgent   = "Nexus/0.1 (+https://github.com/frankbardon/nexus)"
	defaultTimeout     = 20 * time.Second
	defaultMaxSize     = int64(5 * 1024 * 1024) // 5 MB
	defaultSearchCount = 10
	defaultExtractMode = "readability"
)

// Plugin implements the web_search and web_fetch tools.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	replay  *engine.ReplayState

	client *http.Client
	cache  *urlCache
	unsubs []func()

	liveCalls atomic.Uint64

	// search config
	defaultCount      int
	defaultSafeSearch string
	defaultLanguage   string

	// fetch config
	userAgent       string
	timeout         time.Duration
	maxSize         int64
	defaultExtract  string // "readability" | "raw"
	allowedDomains  []string
	blockedDomains  []string
	followRedirects bool
	maxRedirects    int
}

// New returns a fresh web tool plugin.
func New() engine.Plugin {
	return &Plugin{
		defaultCount:      defaultSearchCount,
		defaultSafeSearch: "moderate",
		userAgent:         defaultUserAgent,
		timeout:           defaultTimeout,
		maxSize:           defaultMaxSize,
		defaultExtract:    defaultExtractMode,
		followRedirects:   true,
		maxRedirects:      5,
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Requires declares the search.provider capability. The lifecycle manager
// will fail boot if neither a pin nor an advertising plugin is available,
// which is the right default for a tool that cannot function without a
// search backend. Embedders that want only web_fetch can deactivate this
// plugin and activate a lower-level fetcher instead.
func (p *Plugin) Requires() []engine.Requirement {
	return []engine.Requirement{{Capability: "search.provider"}}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.replay = ctx.Replay

	if err := p.loadConfig(ctx.Config); err != nil {
		return err
	}

	p.client = &http.Client{
		Timeout: p.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !p.followRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) >= p.maxRedirects {
				return fmt.Errorf("stopped after %d redirects", p.maxRedirects)
			}
			return nil
		},
	}
	p.cache = newURLCache()

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.session.end", p.handleSessionEnd,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name: "web_search",
		Description: "Search the web for up-to-date information. Returns a list of results " +
			"with title, URL, and snippet. Use web_fetch afterward to read the full content " +
			"of a specific URL.",
		Class:    "research",
		Subclass: "search",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query.",
				},
				"count": map[string]any{
					"type":        "integer",
					"description": "Max number of results (default 10).",
				},
				"freshness": map[string]any{
					"type":        "string",
					"description": "Restrict to recent results: 'day', 'week', or 'month'. Provider-dependent.",
					"enum":        []string{"day", "week", "month"},
				},
				"language": map[string]any{
					"type":        "string",
					"description": "BCP-47 language tag (e.g. 'en', 'en-US'). Provider-dependent.",
				},
			},
			"required": []string{"query"},
		},
	})

	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name: "web_fetch",
		Description: "Fetch a web page and return its main article text (via readability " +
			"extraction). Use extract='raw' for documentation, tables, or non-article pages.",
		Class:    "research",
		Subclass: "fetch",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute http(s) URL to fetch.",
				},
				"extract": map[string]any{
					"type":        "string",
					"description": "Extraction mode. 'readability' (default) isolates the main article. 'raw' returns the full page text.",
					"enum":        []string{"readability", "raw"},
				},
			},
			"required": []string{"url"},
		},
	})

	return nil
}

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
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "io.session.end", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
		"tool.register",
		"search.request",
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.Name != "web_search" && tc.Name != "web_fetch" {
		return
	}
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}
	p.liveCalls.Add(1)
	switch tc.Name {
	case "web_search":
		p.handleSearch(tc)
	case "web_fetch":
		p.handleFetch(tc)
	}
}

// LiveCalls returns the count of web_search/web_fetch invocations that
// survived the replay short-circuit. Tests assert zero during replay.
func (p *Plugin) LiveCalls() uint64 { return p.liveCalls.Load() }

func (p *Plugin) handleSessionEnd(_ engine.Event[any]) {
	if p.cache != nil {
		p.cache.clear()
	}
}

func (p *Plugin) loadConfig(cfg map[string]any) error {
	search, _ := cfg["search"].(map[string]any)
	if search != nil {
		if n, ok := toInt(search["count"]); ok && n > 0 {
			p.defaultCount = n
		}
		if s, ok := search["safe_search"].(string); ok && s != "" {
			p.defaultSafeSearch = s
		}
		if s, ok := search["language"].(string); ok {
			p.defaultLanguage = s
		}
	}

	fetch, _ := cfg["fetch"].(map[string]any)
	if fetch != nil {
		if s, ok := fetch["user_agent"].(string); ok && s != "" {
			p.userAgent = s
		}
		if s, ok := fetch["timeout"].(string); ok {
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("web: invalid fetch.timeout %q: %w", s, err)
			}
			p.timeout = d
		}
		if s, ok := fetch["max_size"].(string); ok {
			n, err := parseBytes(s)
			if err != nil {
				return fmt.Errorf("web: invalid fetch.max_size %q: %w", s, err)
			}
			p.maxSize = n
		}
		if s, ok := fetch["extract_mode"].(string); ok && s != "" {
			if s != "readability" && s != "raw" {
				return fmt.Errorf("web: invalid fetch.extract_mode %q (want readability|raw)", s)
			}
			p.defaultExtract = s
		}
		if b, ok := fetch["follow_redirects"].(bool); ok {
			p.followRedirects = b
		}
		if n, ok := toInt(fetch["max_redirects"]); ok && n > 0 {
			p.maxRedirects = n
		}
		p.allowedDomains = normalizeDomains(fetch["allowed_domains"])
		p.blockedDomains = normalizeDomains(fetch["blocked_domains"])
	}
	return nil
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{
		ID:     tc.ID,
		Name:   tc.Name,
		Output: output,
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// parseBytes accepts values like "5MB", "500KB", or a bare byte count.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "KB"):
		mult = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	s = strings.TrimSpace(s)
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return n * mult, nil
}

func normalizeDomains(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok || s == "" {
			continue
		}
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}
