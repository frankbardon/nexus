package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// handleSearch services a web_search tool call by emitting search.request
// and formatting whatever the provider filled in. Works for every adapter
// registered under the search.provider capability (Brave, Tavily, native
// Anthropic/OpenAI, future plugins).
func (p *Plugin) handleSearch(tc events.ToolCall) {
	query, _ := tc.Arguments["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		p.emitResult(tc, "", "query argument is required")
		return
	}

	req := &events.SearchRequest{SchemaVersion: events.SearchRequestVersion, Query: query,
		Count:      p.defaultCount,
		SafeSearch: p.defaultSafeSearch,
		Language:   p.defaultLanguage,
	}
	if n, ok := toInt(tc.Arguments["count"]); ok && n > 0 {
		req.Count = n
	}
	if s, ok := tc.Arguments["freshness"].(string); ok {
		req.Freshness = s
	}
	if s, ok := tc.Arguments["language"].(string); ok && s != "" {
		req.Language = s
	}

	if err := p.bus.Emit("search.request", req); err != nil {
		p.emitResult(tc, "", fmt.Sprintf("search dispatch failed: %s", err))
		return
	}

	if req.Error != "" {
		provider := req.Provider
		if provider == "" {
			provider = "unknown"
		}
		p.emitResult(tc, "", fmt.Sprintf("search failed (%s): %s", provider, req.Error))
		return
	}
	if req.Provider == "" {
		p.emitResult(tc, "", "no search provider answered — check that a plugin advertising 'search.provider' is active")
		return
	}

	p.emitResult(tc, formatSearchResults(req), "")
}

// formatSearchResults renders results in a compact, LLM-friendly shape:
// one entry per result with title, URL, snippet, and (when available) date.
// The same payload is also available as JSON in the structured output for
// consumers that want to parse it.
func formatSearchResults(req *events.SearchRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q (via %s, %d result(s)):\n\n", req.Query, req.Provider, len(req.Results))
	if len(req.Results) == 0 {
		b.WriteString("(no results)\n")
		return b.String()
	}
	for i, r := range req.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
		if !r.PublishedAt.IsZero() {
			fmt.Fprintf(&b, "   published: %s\n", r.PublishedAt.Format("2006-01-02"))
		}
		if snip := strings.TrimSpace(r.Snippet); snip != "" {
			fmt.Fprintf(&b, "   %s\n", snip)
		}
		b.WriteString("\n")
	}

	// Append machine-readable JSON for programmatic consumers.
	payload := map[string]any{
		"query":    req.Query,
		"provider": req.Provider,
		"results":  req.Results,
	}
	if raw, err := json.Marshal(payload); err == nil {
		b.WriteString("---\n")
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}
